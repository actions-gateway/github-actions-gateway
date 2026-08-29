package provisioner

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// auditWaiter builds a waiter in the given mode whose log stream is captured, so
// a test can assert on the records it wrote — and, just as load-bearing here, on
// the ones it did not.
func auditWaiter(mode WorkerAuditMode) (*InformerPodWaiter, *lockedBuffer) {
	buf := &lockedBuffer{}
	w := newTestWaiter()
	w.log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	w.WorkerAudit = mode
	return w, buf
}

// auditRecords decodes every worker-address record in a captured log stream.
func auditRecords(t *testing.T, buf *lockedBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		if rec["msg"] == workerAuditMsg {
			out = append(out, rec)
		}
	}
	return out
}

// addressedPod is a worker pod the CNI has given an address, carrying the job
// annotations the provisioner stamps at creation.
func addressedPod(ns, name, ip string) *corev1.Pod {
	p := pod(ns, name, corev1.PodRunning, "")
	p.Status.PodIP = ip
	p.Annotations = map[string]string{
		AnnotationRunID:      "12345678",
		AnnotationRepository: "owner/repo",
	}
	return p
}

// TestWorkerAudit_OffWritesNothing is the control for every assertion below. The
// default mode must write no record even for a pod that has an address and a
// job, because the record is half of an attribution join the platform opts into.
func TestWorkerAudit_OffWritesNothing(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditOff)

	p := addressedPod("ns", "p", "10.1.2.3")
	w.onPodEvent(p, false)
	w.onPodDelete(p)

	if recs := auditRecords(t, buf); len(recs) != 0 {
		t.Fatalf("audit Off wrote %d worker-address records, want 0", len(recs))
	}
}

// TestWorkerAudit_ZeroValueIsOff pins that a waiter nobody configured records
// nothing: the off-by-default guarantee must not depend on main.go having run.
func TestWorkerAudit_ZeroValueIsOff(t *testing.T) {
	var w InformerPodWaiter
	if w.WorkerAudit == WorkerAuditAddresses {
		t.Fatal("zero value must not be WorkerAddresses")
	}
	if got := WorkerAuditMode(""); got == WorkerAuditAddresses {
		t.Fatal("empty mode must not be WorkerAddresses")
	}
}

// TestWorkerAudit_BindCarriesTenantAndJob is the whole point of the record: an
// address on its own attributes nothing, so the tenant namespace and the job's
// run identity must both be on the line that announces it.
func TestWorkerAudit_BindCarriesTenantAndJob(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	w.onPodEvent(addressedPod("team-a", "worker-1", "10.1.2.3"), false)

	recs := auditRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	for field, want := range map[string]any{
		"namespace":  "team-a",
		"event":      workerAuditEventBind,
		"podIP":      "10.1.2.3",
		"pod":        "worker-1",
		"owner":      "set-a",
		"runID":      "12345678",
		"repository": "owner/repo",
	} {
		if got := recs[0][field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
	if lvl := recs[0]["level"]; lvl != "INFO" {
		t.Errorf("level = %v, want INFO: an audit record must not depend on debug", lvl)
	}
}

// TestWorkerAudit_BindWrittenOncePerPod: the informer delivers many update events
// per pod, and one address is one binding. A record per event would bury the join.
func TestWorkerAudit_BindWrittenOncePerPod(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	p := addressedPod("ns", "p", "10.1.2.3")
	w.onPodEvent(p, false)
	w.onPodEvent(p, false)
	w.onPodEvent(p, false)

	if recs := auditRecords(t, buf); len(recs) != 1 {
		t.Fatalf("got %d bind records for one pod, want 1", len(recs))
	}
}

// TestWorkerAudit_ReleaseBoundsTheWindow: an address returns to the CNI's pool
// when the pod goes, so a bind with no release leaves an unbounded window and a
// later pod's connections could be read as this job's.
func TestWorkerAudit_ReleaseBoundsTheWindow(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	p := addressedPod("ns", "p", "10.1.2.3")
	w.onPodEvent(p, false)
	w.onPodDelete(p)

	recs := auditRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want a bind and a release", len(recs))
	}
	if recs[0]["event"] != workerAuditEventBind || recs[1]["event"] != workerAuditEventRelease {
		t.Fatalf("got events %v then %v, want bind then release", recs[0]["event"], recs[1]["event"])
	}
	if recs[1]["podIP"] != "10.1.2.3" {
		t.Errorf("release podIP = %v, want the address it bounds", recs[1]["podIP"])
	}
}

// TestWorkerAudit_RebindsAfterRelease: a pod's claim is released with the pod, so
// an address reused by a later pod is announced again. Without this a restarted
// or recreated worker's traffic would file under whichever pod bound first.
func TestWorkerAudit_RebindsAfterRelease(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	first := addressedPod("ns", "p1", "10.1.2.3")
	w.onPodEvent(first, false)
	w.onPodDelete(first)

	second := addressedPod("ns", "p2", "10.1.2.3")
	second.UID = types.UID("ns/p2-distinct")
	w.onPodEvent(second, false)

	recs := auditRecords(t, buf)
	if len(recs) != 3 {
		t.Fatalf("got %d records, want bind, release, bind", len(recs))
	}
	if recs[2]["event"] != workerAuditEventBind || recs[2]["pod"] != "p2" {
		t.Fatalf("third record = %v, want a bind for p2", recs[2])
	}
}

// TestWorkerAudit_RebindsOnRestart: a pod already running when this process
// starts arrives in the informer's initial list. It is announced anyway — the
// opposite of how the histograms retire an initial-list pod, because the failure
// modes are not symmetric. A duplicate bind is one extra line joining to the same
// job; a missing one leaves the rest of that job's egress unattributable.
func TestWorkerAudit_RebindsOnRestart(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	w.onPodEvent(addressedPod("ns", "p", "10.1.2.3"), true)

	if recs := auditRecords(t, buf); len(recs) != 1 {
		t.Fatalf("got %d records for an initial-list worker pod, want 1", len(recs))
	}
}

// TestWorkerAudit_SkipsPodWithNoAddress: a Pending pod has nothing to bind, and a
// record naming an empty address would join to every connection or to none.
func TestWorkerAudit_SkipsPodWithNoAddress(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	w.onPodEvent(pod("ns", "p", corev1.PodPending, ""), false)

	if recs := auditRecords(t, buf); len(recs) != 0 {
		t.Fatalf("got %d records for an address-less pod, want 0", len(recs))
	}
}

// TestWorkerAudit_SkipsNonWorkerPod: the tenant namespace also holds the AGC's own
// pod, whose egress shares the pool. Leaving it unbound is what lets an auditor
// tell control-plane traffic from a worker's — a bind record for it would file the
// AGC's own connections as a job's.
func TestWorkerAudit_SkipsNonWorkerPod(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	p := addressedPod("ns", "agc", "10.1.2.9")
	p.Labels = nil
	w.onPodEvent(p, false)

	if recs := auditRecords(t, buf); len(recs) != 0 {
		t.Fatalf("got %d records for a non-worker pod, want 0", len(recs))
	}
}

// TestWorkerAudit_BindWithoutJobAnnotations: a scale-set worker whose payload
// carried no run identity still needs its address bound, because the
// address-to-namespace half is what attributes a shared pool and does not depend
// on the job fields.
func TestWorkerAudit_BindWithoutJobAnnotations(t *testing.T) {
	w, buf := auditWaiter(WorkerAuditAddresses)

	p := addressedPod("team-a", "worker-1", "10.1.2.3")
	p.Annotations = nil
	w.onPodEvent(p, false)

	recs := auditRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0]["namespace"] != "team-a" || recs[0]["podIP"] != "10.1.2.3" {
		t.Fatalf("record lost the address-to-tenant half: %v", recs[0])
	}
	if _, present := recs[0]["runID"]; present {
		t.Error("an absent annotation must be omitted, not written empty")
	}
}

func TestParseWorkerAuditMode(t *testing.T) {
	// Everything unrecognized must resolve to Off: a GMC newer than the AGC image
	// can inject a mode this binary does not know, and under-recording is the only
	// safe direction.
	for _, in := range []string{"", "  ", "off", "Off", "OFF", "on", "true", "1", "Addresses", "Workers", "WorkerAddress"} {
		if got := ParseWorkerAuditMode(in); got != WorkerAuditOff {
			t.Errorf("ParseWorkerAuditMode(%q) = %q, want Off", in, got)
		}
	}
	for _, in := range []string{"WorkerAddresses", "workeraddresses", "  WorkerAddresses  "} {
		if got := ParseWorkerAuditMode(in); got != WorkerAuditAddresses {
			t.Errorf("ParseWorkerAuditMode(%q) = %q, want WorkerAddresses", in, got)
		}
	}
}
