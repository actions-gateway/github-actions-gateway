//go:build rwxstorage

// The live half of the shared worker storage validation (Q719).
//
// worker_shared_storage_test.go pins what the provisioner emits. It cannot say
// whether that pod actually mounts a ReadWriteMany volume, because the property
// belongs to a kubelet, a CSI driver and two nodes — none of which envtest has.
// This file takes the pod the provisioner really builds and runs it, twice, on
// two different nodes of the kind cluster scripts/e2e/rwx-storage-cluster.sh
// stands up, against a real csi-driver-nfs class.
//
// It is deliberately NOT in `make check` or per-PR CI: it costs a cluster. Run it
// when changing the worker pod's volume or security handling, and on the cadence
// docs/development/testing.md sets. The reference architecture it validates, and
// the storage classes it has been exercised against, are in
// docs/operations/worker-shared-storage.md.
package provisioner_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// podBudget bounds every wait. A cold node pulls the probe image before the
// container can report anything, and the rendezvous below adds a poll interval on
// top; the rest is headroom for a loaded machine.
const podBudget = 5 * time.Minute

// probeImage is a shell the assertions can run. It is not the runner image: the
// property under test is the pod SHAPE the provisioner emits — its volumes, its
// mounts and its security context — and the real runner would need a broker and a
// job to do anything at all. Named on the podTemplate rather than swapped in
// afterwards, so the pod that reaches the cluster is one buildPod produced.
const probeImage = "registry.k8s.io/e2e-test-images/busybox:1.36.1-1"

// liveRWXClient connects to the harness cluster. It FAILS rather than skips when
// the cluster is absent: this file only compiles under an explicit build tag, so
// getting here means someone asked for the live check.
func liveRWXClient(t *testing.T) client.Client {
	t.Helper()

	kubeContext := os.Getenv("RWX_KUBE_CONTEXT")
	if kubeContext == "" {
		cluster := os.Getenv("RWX_STORAGE_CLUSTER")
		if cluster == "" {
			cluster = "gag-rwx"
		}
		kubeContext = "kind-" + cluster
	}

	cfg, err := ctrlconfig.GetConfigWithContext(kubeContext)
	require.NoErrorf(t, err, "no kubeconfig for context %q — run `make rwx-storage-cluster` first", kubeContext)

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoErrorf(t, err, "could not reach context %q — run `make rwx-storage-cluster` first", kubeContext)
	return c
}

func rwxStorageClass() string {
	if sc := os.Getenv("RWX_STORAGE_CLASS"); sc != "" {
		return sc
	}
	return "gag-rwx-nfs"
}

// liveNS creates a namespace for one test and deletes it afterwards, so a re-run
// never inherits a previous run's pods or the volume they wrote into.
func liveNS(t *testing.T, c client.Client, prefix string) string {
	t.Helper()

	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	require.NoError(t, c.Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	t.Cleanup(func() {
		// Best-effort: a failed cleanup must not mask the assertion that failed.
		_ = c.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return name
}

// schedulableNodes returns the cluster's non-control-plane nodes. Two are
// required: the property under test is cross-node sharing, and two pods on one
// kubelet would be satisfied by an RWO volume too.
func schedulableNodes(t *testing.T, c client.Client) []string {
	t.Helper()

	var nodes corev1.NodeList
	require.NoError(t, c.List(context.Background(), &nodes))
	var out []string
	for i := range nodes.Items {
		if _, isCP := nodes.Items[i].Labels["node-role.kubernetes.io/control-plane"]; isCP {
			continue
		}
		out = append(out, nodes.Items[i].Name)
	}
	require.GreaterOrEqualf(t, len(out), 2, "the harness needs two schedulable nodes; got %v", out)
	return out
}

// createRWXClaim creates the shared claim the workers mount and waits for it to
// bind. A claim that never binds is a harness fault, not a finding, so it fails
// with the class named.
func createRWXClaim(t *testing.T, c client.Client, ns string) {
	t.Helper()

	sc := rwxStorageClass()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: sharedClaimName, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To(sc),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	require.NoError(t, c.Create(context.Background(), pvc))

	deadline := time.Now().Add(podBudget)
	for {
		var got corev1.PersistentVolumeClaim
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &got))
		if got.Status.Phase == corev1.ClaimBound {
			// The class is only interesting if it granted what was asked for.
			require.Contains(t, got.Status.AccessModes, corev1.ReadWriteMany,
				"storage class %q bound the claim without ReadWriteMany", sc)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim %s/%s never bound on storage class %q within %s (phase %q)",
				ns, sharedClaimName, sc, podBudget, got.Status.Phase)
		}
		time.Sleep(2 * time.Second)
	}
}

// buildWorkerPod runs the real provision path against a fake client and returns
// the worker pod it built, with `command` as the runner container's argv. Nothing
// about the pod is edited afterwards beyond what placing it in another cluster
// requires (identity, owner, node), so what the assertions run is what the AGC
// would create.
func buildWorkerPod(t *testing.T, profile string, fsGroup *int64, planID string, command []string) *corev1.Pod {
	t.Helper()

	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.SecurityProfile = profile

	rg := newRG("mygroup", "team-a")
	rg.Spec.PodTemplate = sharedStorageTemplate(fsGroup)
	rg.Spec.PodTemplate.Spec.Containers[0].Image = probeImage
	rg.Spec.PodTemplate.Spec.Containers[0].Command = command

	return runAndGetPod(ctx, t, p, fc, rg, planID, "team-a")
}

// placeWorkerPod re-homes a built worker pod into the live namespace on `node`
// and creates it, along with the payload Secret it mounts — which the AGC creates
// beside every worker and whose absence would leave the pod in
// ContainerCreating forever, indistinguishable from a storage fault.
func placeWorkerPod(t *testing.T, c client.Client, ns, node, name string, built *corev1.Pod) *corev1.Pod {
	t.Helper()

	ctx := context.Background()
	for i := range built.Spec.Volumes {
		src := built.Spec.Volumes[i].VolumeSource.Secret
		if src == nil {
			continue
		}
		err := c.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: src.SecretName, Namespace: ns},
			Data:       map[string][]byte{"payload": []byte("{}")},
		})
		require.NoError(t, err)
	}

	pod := built.DeepCopy()
	pod.ObjectMeta = metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		Labels:    built.Labels,
	}
	pod.Spec.NodeName = node
	pod.Status = corev1.PodStatus{}
	require.NoError(t, c.Create(ctx, pod))
	return pod
}

// awaitTerminated blocks until the pod's runner container has exited, and returns
// its terminated state — the exit code and whatever it wrote to the termination
// log, which is how each probe reports what the filesystem said.
func awaitTerminated(t *testing.T, c client.Client, pod *corev1.Pod) *corev1.ContainerStateTerminated {
	t.Helper()

	deadline := time.Now().Add(podBudget)
	for {
		var got corev1.Pod
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pod), &got))
		for i := range got.Status.ContainerStatuses {
			cs := &got.Status.ContainerStatuses[i]
			if cs.Name == "runner" && cs.State.Terminated != nil {
				return cs.State.Terminated
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s/%s never terminated within %s (phase %q, conditions %+v)",
				pod.Namespace, pod.Name, podBudget, got.Status.Phase, got.Status.Conditions)
		}
		time.Sleep(2 * time.Second)
	}
}

// nodeNameOf returns the node the apiserver says ran this pod.
func nodeNameOf(t *testing.T, c client.Client, pod *corev1.Pod) string {
	t.Helper()

	var got corev1.Pod
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pod), &got))
	return got.Spec.NodeName
}

// TestLiveRWX_TwoWorkersShareOneVolume is the validation proper: two workers of
// one tenant, on two different nodes, exchanging files through one ReadWriteMany
// claim. Each writes its own file and then blocks until it can read the other's,
// so a pass requires traffic in both directions — a volume that silently gave each
// pod its own copy would hang both halves rather than passing.
func TestLiveRWX_TwoWorkersShareOneVolume(t *testing.T) {
	c := liveRWXClient(t)
	ns := liveNS(t, c, "q719-share")
	createRWXClaim(t, c, ns)
	nodes := schedulableNodes(t, c)

	// Write my own name into my own file, then block until the other worker's file
	// holds the other worker's name. Matching on content rather than existence:
	// an empty file, or one the mount created itself, would satisfy `test -f`.
	rendezvous := func(mine, theirs string) []string {
		return []string{"sh", "-c", fmt.Sprintf(
			"set -e; echo %s > /mnt/shared/%s; "+
				"while ! grep -q %s /mnt/shared/%s 2>/dev/null; do sleep 1; done; "+
				"echo saw-%s > /dev/termination-log", mine, mine, theirs, theirs, theirs)}
	}

	a := placeWorkerPod(t, c, ns, nodes[0], "worker-a",
		buildWorkerPod(t, "restricted", ptr.To(int64(1001)), "plan-rwx-live-a", rendezvous("a", "b")))
	b := placeWorkerPod(t, c, ns, nodes[1], "worker-b",
		buildWorkerPod(t, "restricted", ptr.To(int64(1001)), "plan-rwx-live-b", rendezvous("b", "a")))

	for _, w := range []*corev1.Pod{a, b} {
		term := awaitTerminated(t, c, w)
		assert.Equalf(t, int32(0), term.ExitCode,
			"worker %s did not complete the exchange: %s", w.Name, term.Message)
	}

	// Read the placement back from the cluster rather than from the pods this test
	// built: what has to be true is that two kubelets ran these containers, and the
	// nodeName this process set proves only what it asked for.
	assert.NotEqual(t, nodeNameOf(t, c, a), nodeNameOf(t, c, b),
		"both workers ran on one node, so the exchange says nothing about shared storage")
}

// TestLiveRWX_WithoutFSGroupTheRunnerCannotWrite is the control that gives the
// test above its meaning, and the measurement behind the reference architecture's
// one hard requirement. A freshly provisioned volume's root belongs to root; the
// worker's gap-filled UID 1001 is not root and is in no group that owns it. Without
// the fsGroup that fixes that, the mount succeeds and the first write fails — the
// failure mode an operator meets as a job that starts fine and dies mid-step.
//
// If this ever passes, fsGroup has stopped being load-bearing and
// docs/operations/worker-shared-storage.md is telling operators to set a field
// that does nothing.
func TestLiveRWX_WithoutFSGroupTheRunnerCannotWrite(t *testing.T) {
	c := liveRWXClient(t)
	ns := liveNS(t, c, "q719-nofsgroup")
	createRWXClaim(t, c, ns)
	nodes := schedulableNodes(t, c)

	probe := []string{"sh", "-c", "touch /mnt/shared/probe 2>/dev/termination-log"}
	w := placeWorkerPod(t, c, ns, nodes[0], "worker-nofsgroup",
		buildWorkerPod(t, "restricted", nil, "plan-rwx-live-nofsgroup", probe))

	term := awaitTerminated(t, c, w)
	require.NotEqual(t, int32(0), term.ExitCode,
		"the runner UID wrote to a root-owned shared volume with no fsGroup; "+
			"either the storage class is pre-relaxing permissions or the worker is not running as the UID it reports")
	assert.Contains(t, term.Message, "Permission denied",
		"the write must fail on permissions specifically — any other error means this control "+
			"is passing for a reason unrelated to volume ownership")
}
