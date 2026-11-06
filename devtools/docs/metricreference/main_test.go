package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A synthetic AGC tree and the two docs that describe it, all six checks green.
// Each test mutates exactly one input and asserts the finding it should produce,
// so every case starts from a green baseline: a check that stopped firing would
// otherwise be indistinguishable from a fixture that never tripped it.
//
// The tree reproduces the three shapes the real source uses — a shared metrics
// struct written from another package, a scale-set-only struct, and a scrape-time
// collector built on prometheus.NewDesc.
const (
	coreSrc = `package runnercore

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	JobsAcquired *prometheus.CounterVec
	PodsReaped   *prometheus.CounterVec
	Retries      *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	return &Metrics{
		JobsAcquired: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_jobs_acquired_total",
			Help: "h",
		}, []string{"namespace"}),
		PodsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_pods_reaped_total",
			Help: "h",
		}, []string{"namespace"}),
		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_retries_total",
			Help: "h, by cause (eviction, vanished, abandoned).",
		}, []string{"namespace", "tier", "cause"}),
	}
}
`

	// The label-value shape the real tree has: the counter is written in a helper
	// whose cause parameter the callers name, one of them from a scale-set-only
	// file. Nothing about the emission site says which tier a cause reaches — the
	// call that names the constant is what does.
	evictionSrc = `package provisioner

const (
	causeEviction = "eviction"
	causeVanished = "vanished"
	causeAbandoned = "abandoned"
)

func (p *P) handleEviction(tier, cause string) {
	p.Metrics.Retries.WithLabelValues(p.ns, tier, cause).Inc()
}

func (p *P) direct() {
	p.handleEviction("scaleset", causeEviction)
	p.handleEviction("scaleset", causeAbandoned)
}
`
	orphanedScaleSetSrc = `package provisioner

func (p *P) orphaned() { p.handleEviction("scaleset", causeVanished) }
`
	// A second package writing the acquire counter.
	listenerSrc = `package listener

func (l *L) run() {
	l.Metrics.JobsAcquired.WithLabelValues(l.ns).Inc()
}
`
	sharedSrc = `package controller

func (r *R) reap() {
	r.Metrics.PodsReaped.WithLabelValues(r.ns).Inc()
}
`
	scaleSetSrc = `package scalesetlistener

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	Assigned *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	m := &Metrics{
		Assigned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_assigned_total",
			Help: "h",
		}, []string{"namespace"}),
	}
	m.Assigned.WithLabelValues("ns").Inc()
	return m
}
`
	collectorSrc = `package controller

import "github.com/prometheus/client_golang/prometheus"

type c struct{ pressure *prometheus.Desc }

func newC() *c {
	return &c{
		pressure: prometheus.NewDesc(
			"actions_gateway_worker_quota_pressure", "h", []string{"namespace"}, nil,
		),
	}
}

func (x *c) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(x.pressure, prometheus.GaugeValue, 1)
}
`

	// Every metric described.
	goodReference = "# Metrics\n\n" +
		"| Metric | Description |\n| --- | --- |\n" +
		"| `actions_gateway_jobs_acquired_total` | Jobs acquired. |\n" +
		"| `actions_gateway_worker_pods_reaped_total` | Pods reaped. |\n" +
		"| `actions_gateway_scaleset_jobs_assigned_total` | Jobs assigned. |\n" +
		"| `actions_gateway_worker_quota_pressure` | Quota pressure. |\n" +
		"| `actions_gateway_eviction_retries_total` | Recoveries started. |\n\n"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// tree writes the synthetic AGC source, applying any per-file overrides. An
// override with an empty body drops the file.
func tree(t *testing.T, overrides map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"internal/runnercore/metrics.go":            coreSrc,
		"internal/listener/goroutine.go":            listenerSrc,
		"internal/controller/runner_shared.go":      sharedSrc,
		"internal/scalesetlistener/metrics.go":      scaleSetSrc,
		"internal/controller/worker_quota.go":       collectorSrc,
		"internal/provisioner/eviction.go":          evictionSrc,
		"internal/provisioner/orphaned_scaleset.go": orphanedScaleSetSrc,
	}
	for name, body := range overrides {
		files[name] = body
	}
	for name, body := range files {
		if body == "" {
			continue
		}
		writeFile(t, filepath.Join(dir, name), body)
	}
	return dir
}

// runCase writes the doc and returns the findings.
func runCase(t *testing.T, srcDir, metricsDoc string) []string {
	t.Helper()
	mPath := filepath.Join(t.TempDir(), "observability-metrics.md")
	writeFile(t, mPath, metricsDoc)
	findings, err := run(srcDir, mPath)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return findings
}

func requireFinding(t *testing.T, findings []string, want string) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f, want) {
			return
		}
	}
	t.Fatalf("no finding containing %q; got %v", want, findings)
}

func TestCleanTreeHasNoFindings(t *testing.T) {
	findings := runCase(t, tree(t, nil), goodReference)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// which an operator cannot tell from a quiet system.
func TestRegisteredButNeverEmittedFails(t *testing.T) {
	src := tree(t, map[string]string{"internal/controller/runner_shared.go": "package controller\n"})
	findings := runCase(t, src, goodReference)
	requireFinding(t, findings, "actions_gateway_worker_pods_reaped_total (field PodsReaped) is registered and never emitted")
}

// A metric documented by its tier and nothing else gives an operator no way to
// read it. eviction_recovery_evidence_lost_total shipped that way (Q809).
func TestMetricWithNoReferenceEntryFails(t *testing.T) {
	reference := strings.Replace(goodReference,
		"| `actions_gateway_worker_quota_pressure` | Quota pressure. |\n", "", 1)
	findings := runCase(t, tree(t, nil), reference)
	requireFinding(t, findings, "actions_gateway_worker_quota_pressure (defined in")
	requireFinding(t, findings, "has no entry")
}

// A scan that matched nothing looks exactly like a clean tree, so an empty
// inventory must refuse rather than report green.
func TestEmptySourceTreeIsAnError(t *testing.T) {
	mPath := filepath.Join(t.TempDir(), "observability-metrics.md")
	writeFile(t, mPath, goodReference)
	if _, err := run(t.TempDir(), mPath); err == nil {
		t.Fatal("expected an error when the source tree defines no metrics")
	}
}

// Test files describe metrics that are never shipped, so they must not enter the
// inventory.
func TestTestFilesAreNotScanned(t *testing.T) {
	stray := `package listener

func TestX() {
	_ = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "actions_gateway_only_in_tests_total"})
}
`
	src := tree(t, map[string]string{"internal/listener/goroutine_test.go": stray})
	findings := runCase(t, src, goodReference)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// Help is what an operator reads off /metrics with no docs open, so a vocabulary
// the code has outgrown reads there as a value that cannot occur.
func TestHelpMissingAnEmittedValueFails(t *testing.T) {
	src := tree(t, map[string]string{
		"internal/runnercore/metrics.go": strings.Replace(coreSrc,
			"h, by cause (eviction, vanished, abandoned).", "h, by cause (eviction, abandoned).", 1),
	})
	findings := runCase(t, src, goodReference)
	requireFinding(t, findings, `emits cause="vanished"`)
	requireFinding(t, findings, "its Help does not name it")
}

func TestBuildInfoValuesAreNotHeldToHelp(t *testing.T) {
	src := tree(t, map[string]string{"internal/core/buildinfo.go": `package core

var buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "actions_gateway_build_info",
	Help: "Build identity.",
}, []string{"component"})

func record() { buildInfo.WithLabelValues("agc").Set(1) }
`})
	reference := goodReference + "| `actions_gateway_build_info` | Build identity. |\n"
	if findings := runCase(t, src, reference); len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// The seam that sets the resolution bound: a shared counter is reached through a
// wrapper and a classifier, so a derivation that stops early misses the value.
func TestValueIsDerivedThroughAWrapperAndClassifier(t *testing.T) {
	src := tree(t, map[string]string{
		"internal/provisioner/orphaned_scaleset.go": `package provisioner

func classify() string { return causeEviction }

func (p *P) orphaned() {
	p.handleEviction("scaleset", causeVanished)
	p.wrap(classify())
}

func (p *P) wrap(cause string) { p.handleEviction("scaleset", cause) }
`,
	})
	files, _, parsed, err := scanSource(src)
	if err != nil {
		t.Fatalf("scanSource: %v", err)
	}
	values, _ := deriveValues(parsed, files)
	var origins []string
	for _, v := range values["actions_gateway_eviction_retries_total"]["cause"] {
		if v.value == "eviction" {
			origins = v.origins
		}
	}
	if len(origins) < 2 {
		t.Fatalf("eviction must be derived from both the shared file and the wrapper's caller, got %v", origins)
	}
}

// The retro behind #1530: 58 of the AGC's 580 function names have more than one
// declaration, so reading argument N off a call placed by name alone can invent a
// value. An unplaceable call is reported because under-derivation is the silent
// failure — a value that goes underived also goes undemanded.
func TestAmbiguousCalleeInALabelPositionIsReported(t *testing.T) {
	src := tree(t, map[string]string{
		// A second handleEviction on another receiver in the same package, holding
		// cause at a different index. A call names one of the two and only the
		// receiver's type says which, so neither can be placed.
		"internal/provisioner/eviction_twin.go": `package provisioner

func (q *Q) handleEviction(cause, tier string) { _ = cause }

func (q *Q) drive() { q.handleEviction("some_reason", "scaleset") }
`,
	})
	findings := runCase(t, src, goodReference)
	requireFinding(t, findings, "handleEviction has more than one declaration")
	for _, f := range findings {
		if strings.Contains(f, `cause="some_reason"`) {
			t.Fatalf("a value from the same-named twin must not be attributed: %s", f)
		}
	}
}
