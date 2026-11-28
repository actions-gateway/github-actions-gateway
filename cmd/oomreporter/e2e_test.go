//go:build e2e

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/kernlog"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/rule"
)

// The lane's cluster, as scripts/e2e/oomreporter-e2e.sh makes it: the chart installed, one
// worker whose pods share about 1 GiB, and the hog's image loaded.
var (
	e2eKubeconfig = os.Getenv("OOM_E2E_KUBECONFIG")
	e2eWorker     = os.Getenv("OOM_E2E_WORKER")
	e2eNamespace  = os.Getenv("OOM_E2E_NAMESPACE")
	e2eHog        = os.Getenv("OOM_E2E_HOG_IMAGE")
	// e2eReporter is the chart's namespace. The lane installs it with kernelLog on.
	e2eReporter = os.Getenv("OOM_E2E_RELEASE_NAMESPACE")
)

// The cgroup kind's kubelet puts the node's pods in, and the one above it.
const (
	nodePods  = "/sys/fs/cgroup/kubelet.slice/kubelet-kubepods.slice"
	abovePods = "/sys/fs/cgroup/kubelet.slice"
)

// errNotOOMKilled is a hog the runtime ended with another reason, which the reporter answers
// with nothing (q1403-oom-kill-reporter.md § The rule), so the kill set up nothing to test.
var errNotOOMKilled = errors.New("which the runtime did not call OOMKilled")

// kills bounds the hogs each limit gets. One memory kill in 20 ended Error, 137 on a local kind
// cluster (Q1412), so three such ends in a row would be about 1 in 8,000 if they were independent.
const kills = 3

func TestE2E_SaysWhichLimitRanOut(t *testing.T) {
	require.NotEmpty(t, e2eKubeconfig, "OOM_E2E_KUBECONFIG")
	cfg, err := clientcmd.BuildConfigFromFlags("", e2eKubeconfig)
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		reason rule.Reason
		// level is where the kernel's line names the limit.
		level  cgroup.Level
		pod    func(*corev1.Pod)
		around func(t *testing.T)
	}{
		{"container", rule.ContainerLimit, cgroup.InPod, func(p *corev1.Pod) {
			p.Spec.Containers[0].Args = []string{"256"}
			p.Spec.Containers[0].Resources.Limits = memory("64Mi")
			p.Spec.Containers[1].Resources.Limits = memory("32Mi")
		}, nil},
		{"pod", rule.PodLimit, cgroup.PodLevel, func(p *corev1.Pod) {
			p.Spec.Containers[0].Args = []string{"512"}
			p.Spec.Resources = &corev1.ResourceRequirements{Limits: memory("128Mi")}
		}, nil},
		{"pods", rule.PodsLimit, cgroup.RootLevel, func(p *corev1.Pod) {
			p.Spec.Containers[0].Args = []string{"2048"}
		}, func(t *testing.T) {
			require.Less(t, readBytes(t, nodePods+"/memory.max"), int64(1536)<<20, "the worker's pods share less than the hog takes")
		}},
		{"above", rule.AbovePods, cgroup.Above, func(p *corev1.Pod) {
			p.Spec.Containers[0].Args = []string{"2048"}
		}, limitAbovePods},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.around != nil {
				tc.around(t)
			}
			for kill := 1; ; kill++ {
				p := hogPod(fmt.Sprintf("oom-%s-%d", tc.name, kill), tc.pod)
				p, err := kube.CoreV1().Pods(e2eNamespace).Create(t.Context(), p, metav1.CreateOptions{})
				require.NoError(t, err)
				t.Cleanup(func() {
					_ = kube.CoreV1().Pods(e2eNamespace).Delete(context.WithoutCancel(t.Context()), p.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))})
				})
				got, err := waitForCondition(t, kube, p)
				if errors.Is(err, errNotOOMKilled) && kill < kills {
					t.Logf("kill %d of %d is not counted: %v", kill, kills, err)
					continue
				}
				require.NoError(t, err, "kill %d of %d", kill, kills)
				require.Equal(t, string(tc.reason), got.Reason, "%s", got.Message)
				require.Equal(t, corev1.ConditionTrue, got.Status)
				waitForEvent(t, kube, p, "OOM"+string(tc.reason))
				line := waitForLine(t, kube, p)
				require.Equal(t, tc.level, line.Level, "the level the kernel's line names")
				return
			}
		})
	}
}

// hogPod is a pod on the worker whose hog container takes memory beside one that idles.
func hogPod(name string, with func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			NodeName:      e2eWorker,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{Name: "hog", Image: e2eHog, ImagePullPolicy: corev1.PullNever},
				{Name: "idle", Image: e2eHog, ImagePullPolicy: corev1.PullNever, Args: []string{"16"}},
			},
		},
	}
	with(p)
	return p
}

func memory(q string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}
}

// waitForCondition returns the hog's condition once the controller has written it, or
// errNotOOMKilled once the hog has ended another way.
func waitForCondition(t *testing.T, kube kubernetes.Interface, p *corev1.Pod) (corev1.PodCondition, error) {
	t.Helper()
	var got corev1.PodCondition
	var now *corev1.Pod
	err := wait.PollUntilContextTimeout(t.Context(), time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		read, err := kube.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		now = read
		for _, c := range now.Status.ContainerStatuses {
			if term := c.State.Terminated; c.Name == "hog" && term != nil && term.Reason != "OOMKilled" {
				return false, fmt.Errorf("the hog ended %s %d, %w", term.Reason, term.ExitCode, errNotOOMKilled)
			}
		}
		for _, c := range now.Status.Conditions {
			if c.Type == controller.Domain+"/hog" {
				got = c
				return true, nil
			}
		}
		return false, nil
	})
	if errors.Is(err, errNotOOMKilled) {
		return got, err
	}
	require.NoError(t, err, "no condition on %s; its containers: %s", p.Name, states(now))
	return got, nil
}

// states says how each of pod's containers ended, or that it has not.
func states(pod *corev1.Pod) string {
	if pod == nil {
		return "unread"
	}
	var out []string
	for _, c := range pod.Status.ContainerStatuses {
		if term := c.State.Terminated; term != nil {
			out = append(out, fmt.Sprintf("%s %s %d at %s", c.Name, term.Reason, term.ExitCode, term.FinishedAt.UTC().Format(time.RFC3339)))
		} else {
			out = append(out, c.Name+" running")
		}
	}
	return strings.Join(out, ", ")
}

func waitForEvent(t *testing.T, kube kubernetes.Interface, p *corev1.Pod, reason string) {
	t.Helper()
	selector := fields.Set{"involvedObject.uid": string(p.UID), "reason": reason}.AsSelector().String()
	err := wait.PollUntilContextTimeout(t.Context(), time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		events, err := kube.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
		return err == nil && len(events.Items) > 0, err
	})
	require.NoError(t, err, "no Event %s on %s", reason, p.Name)
}

// limitAbovePods puts a limit on the worker's cgroup above its pods, a little over what it
// holds, and takes it off when the test ends. The kubelet and containerd must outlive it.
func limitAbovePods(t *testing.T) {
	was := strings.TrimSpace(node(t, "cat", abovePods+"/memory.max"))
	daemons := node(t, "systemctl", "show", "-p", "MainPID,NRestarts", "kubelet", "containerd")
	limit := readBytes(t, abovePods+"/memory.current") + 256<<20
	require.Less(t, limit, readBytes(t, nodePods+"/memory.max"), "the limit above the pods is below theirs")
	node(t, "sh", "-c", fmt.Sprintf("echo %d > %s/memory.max", limit, abovePods))
	t.Cleanup(func() {
		node(t, "sh", "-c", fmt.Sprintf("echo %s > %s/memory.max", was, abovePods))
		require.Equal(t, daemons, node(t, "systemctl", "show", "-p", "MainPID,NRestarts", "kubelet", "containerd"),
			"the kubelet and containerd outlive the kill")
	})
}

func readBytes(t *testing.T, file string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSpace(node(t, "cat", file)), 10, 64)
	require.NoError(t, err, file)
	return n
}

// node runs a command in the worker's container.
func node(t *testing.T, args ...string) string {
	t.Helper()
	//nolint:gosec // G204: docker, with the lane's own worker and the test's literal arguments.
	out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", append([]string{"exec", e2eWorker}, args...)...).CombinedOutput()
	require.NoError(t, err, "%v: %s", args, out)
	return string(out)
}

// waitForLine returns the line the worker's report holds for the hog's kill, and logs how far its
// time is from the end the kubelet gives the container, which a status has in whole seconds.
func waitForLine(t *testing.T, kube kubernetes.Interface, p *corev1.Pod) report.Line {
	t.Helper()
	pod, err := kube.CoreV1().Pods(p.Namespace).Get(t.Context(), p.Name, metav1.GetOptions{})
	require.NoError(t, err)
	var id string
	var ended time.Time
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == "hog" && c.State.Terminated != nil {
			_, id, _ = strings.Cut(c.State.Terminated.ContainerID, "://")
			ended = c.State.Terminated.FinishedAt.Time
		}
	}
	require.NotEmpty(t, id, "the hog has ended")
	var got report.Line
	err = wait.PollUntilContextTimeout(t.Context(), time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		for _, l := range reportLines(ctx, t, kube) {
			if l.Pod == string(p.UID) && l.Container == id {
				got = l
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, err, "no line in the worker's report for %s", p.Name)
	t.Logf("the line's time is %s from the container's end", time.UnixMilli(got.At).Sub(ended))
	require.InDelta(t, 0, time.UnixMilli(got.At).Sub(ended).Seconds(), 30, "the line's time is the node's")
	return got
}

func reportLines(ctx context.Context, t *testing.T, kube kubernetes.Interface) []report.Line {
	t.Helper()
	cm, err := kube.CoreV1().ConfigMaps(e2eReporter).Get(ctx, report.Name(e2eWorker), metav1.GetOptions{})
	require.NoError(t, err)
	_, lines, err := report.Decode(cm.Data[report.Key])
	require.NoError(t, err)
	return lines
}

// A job that can make cgroups names one for a key with a path, as q1403-oom-kill-reporter.md
// § The kernel log, as an option has it, and is killed in it: the kernel prints task_memcg twice,
// and the reader refuses the line.
func TestE2E_RefusesALineAJobForged(t *testing.T) {
	require.NotEmpty(t, e2eKubeconfig, "OOM_E2E_KUBECONFIG")
	cfg, err := clientcmd.BuildConfigFromFlags("", e2eKubeconfig)
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	forged := []string{"x,task_memcg=", "kubepods.slice", "kubepods-podffffffff_ffff_ffff_ffff_ffffffffffff.slice",
		"cri-containerd-" + strings.Repeat("f", 64) + ".scope"}
	p := hogPod("oom-forged", func(p *corev1.Pod) {
		p.Spec.Containers = p.Spec.Containers[:1]
		p.Spec.Containers[0].Args = append([]string{"256"}, forged...)
		p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: new(true)}
	})
	p, err = kube.CoreV1().Pods(e2eNamespace).Create(t.Context(), p, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kube.CoreV1().Pods(e2eNamespace).Delete(context.WithoutCancel(t.Context()), p.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))})
	})

	reader := agentOnWorker(t, kube)
	err = wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		logs, err := kube.CoreV1().Pods(e2eReporter).GetLogs(reader, &corev1.PodLogOptions{Container: "log-reader"}).DoRaw(ctx)
		return err == nil && strings.Contains(string(logs), kernlog.ErrDuplicate.Error()), nil
	})
	require.NoError(t, err, "the log reader refused no line with a key twice; the hog: %s", hogLogs(t, kube, p))
	for _, l := range reportLines(t.Context(), t, kube) {
		require.NotEqual(t, string(p.UID), l.Pod, "a line for the pod that forged one")
		require.NotEqual(t, "ffffffff-ffff-ffff-ffff-ffffffffffff", l.Pod, "a line for the pod it named")
	}
}

// agentOnWorker is the name of the agent's pod on the worker.
func agentOnWorker(t *testing.T, kube kubernetes.Interface) string {
	t.Helper()
	pods, err := kube.CoreV1().Pods(e2eReporter).List(t.Context(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=agent",
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", e2eWorker).String(),
	})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	return pods.Items[0].Name
}

func hogLogs(t *testing.T, kube kubernetes.Interface, p *corev1.Pod) string {
	logs, err := kube.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: "hog"}).DoRaw(context.WithoutCancel(t.Context()))
	if err != nil {
		return err.Error()
	}
	return string(logs)
}
