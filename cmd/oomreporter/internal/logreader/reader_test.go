package logreader

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const (
	root      = "kubelet.slice/kubelet-kubepods.slice"
	uid       = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	other     = "6fa459ea-ee8a-3ca4-894e-db77e160355e"
	nodePods  = "/docker/c4418cb3a46760163d03c4e46b92727c5dd6c8a99d2a85d45fe7130503ad4142/" + root
	qos       = nodePods + "/kubelet-kubepods-burstable.slice"
	pod       = qos + "/kubelet-kubepods-burstable-pod1b4e28ba_2fa1_11d2_883f_0016d3cca427.slice"
	otherPod  = qos + "/kubelet-kubepods-burstable-pod6fa459ea_ee8a_3ca4_894e_db77e160355e.slice"
	scopeName = "cri-containerd-"
)

var (
	runner  = strings.Repeat("a", 64)
	sidecar = strings.Repeat("b", 64)
	boot    = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
)

func container(pod, id string) string { return pod + "/" + scopeName + id + ".scope" }

// kill is the kernel's two lines for a kill at limit of the victim, its group killed whole, at
// stamp seconds after boot.
func kill(stamp, pid, limit, victim, group string) string {
	l := "<6>[" + stamp + "] oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=x,mems_allowed=0,oom_memcg=" + limit
	if limit == "" {
		l = strings.Replace(l, "CONSTRAINT_MEMCG", "CONSTRAINT_NONE", 1)
		l = strings.Replace(l, ",oom_memcg=", ",global_oom", 1)
	}
	l += ",task_memcg=" + victim + ",task=hog,pid=" + pid + ",uid=0\n"
	if group != "" {
		l += "<6>[" + stamp + "] Tasks in " + group + " are going to be killed due to memory.oom.group set\n"
	}
	return l
}

type fake struct {
	log      string
	sent     []report.Line
	failNext bool
}

func (f *fake) reader(logs *bytes.Buffer) *Reader {
	return &Reader{
		Root: root, Log: func() (string, error) { return f.log, nil },
		Now: func() (time.Time, time.Duration) { return boot.Add(time.Hour), time.Hour },
		Send: func(l report.Line) error {
			if f.failNext {
				f.failNext = false
				return errors.New("no agent")
			}
			f.sent = append(f.sent, l)
			return nil
		},
		Logs: slog.New(slog.NewTextHandler(logs, nil)),
	}
}

func TestReader_SendsNumbersForEachLevel(t *testing.T) {
	runnerCgroup := container(pod, runner)
	for name, tc := range map[string]struct {
		limit string
		want  cgroup.Level
	}{
		"the container's":  {runnerCgroup, cgroup.InPod},
		"the pod's":        {pod, cgroup.PodLevel},
		"the QoS slice's":  {qos, cgroup.QoSLevel},
		"the pods' root's": {nodePods, cgroup.RootLevel},
		"one above":        {strings.TrimSuffix(nodePods, "/kubelet-kubepods.slice"), cgroup.Above},
		"the node's":       {"", cgroup.Above},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{log: kill("30.250000", "7", tc.limit, runnerCgroup, runnerCgroup)}
			require.NoError(t, f.reader(&bytes.Buffer{}).Once())
			assert.Equal(t, []report.Line{{
				Pod: uid, Container: runner, Level: tc.want, At: boot.Add(30*time.Second + 250*time.Millisecond).UnixMilli(),
			}}, f.sent)
		})
	}
}

func TestReader_SendsNothingNoContainerOfTheVictimsWasKilledFor(t *testing.T) {
	runnerCgroup := container(pod, runner)
	for name, log := range map[string]string{
		"no group named":                kill("1.000000", "7", pod, runnerCgroup, ""),
		"the pod's group, not one":      kill("1.000000", "7", pod, runnerCgroup, pod),
		"a group below the container":   kill("1.000000", "7", pod, runnerCgroup+"/nested", runnerCgroup+"/nested"),
		"a victim in no container":      kill("1.000000", "7", pod, pod+"/nested", pod+"/nested"),
		"a limit the kubelet names not": kill("1.000000", "7", qos+"/other.slice", qos+"/other.slice/c", qos+"/other.slice/c"),
		"no stamp":                      strings.ReplaceAll(kill("1.000000", "7", pod, runnerCgroup, runnerCgroup), "[1.000000] ", ""),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{log: log}
			require.NoError(t, f.reader(&bytes.Buffer{}).Once())
			assert.Empty(t, f.sent)
		})
	}
}

func TestReader_SendsALineOnceAndAgainAfterAFailedSend(t *testing.T) {
	a := kill("1.000000", "7", pod, container(pod, runner), container(pod, runner))
	b := kill("2.000000", "8", otherPod, container(otherPod, sidecar), container(otherPod, sidecar))
	f := &fake{log: a}
	r := f.reader(&bytes.Buffer{})
	require.NoError(t, r.Once())
	require.NoError(t, r.Once())
	require.Len(t, f.sent, 1)

	f.log = a + b
	require.NoError(t, r.Once())
	require.Len(t, f.sent, 2)
	assert.Equal(t, other, f.sent[1].Pod)

	c := kill("3.000000", "9", pod, container(pod, sidecar), container(pod, sidecar))
	f.log, f.failNext = a+b+c, true
	require.Error(t, r.Once())
	require.NoError(t, r.Once())
	assert.Len(t, f.sent, 5, "all three again, to an agent that may have restarted")
}

func TestReader_LogsEachRefusalOnce(t *testing.T) {
	forged := strings.Replace(kill("1.000000", "7", pod, container(pod, runner), ""), "<6>", "<4>", 1)
	var logs bytes.Buffer
	f := &fake{log: forged}
	r := f.reader(&logs)
	require.NoError(t, r.Once())
	require.NoError(t, r.Once())
	assert.Equal(t, 1, strings.Count(logs.String(), "refused a line"))
	assert.NotContains(t, logs.String(), "oom-kill:", "no text of the line")
}

func TestReader_RefusesNoReadOffLinux(t *testing.T) {
	if _, err := os.Stat("/proc/kmsg"); err == nil {
		t.Skip("on Linux")
	}
	_, err := ReadAll()
	assert.Error(t, err)
	wall, _ := Now()
	assert.False(t, wall.IsZero())
}
