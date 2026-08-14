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

func (p *P) classic() {
	p.handleEviction("classic", causeEviction)
	p.handleEviction("classic", causeAbandoned)
}
`
	orphanedScaleSetSrc = `package provisioner

func (p *P) orphaned() { p.handleEviction("scaleset", causeVanished) }
`
	// The classic listener writes the acquire counter; the shared reconciler
	// writes the reap counter, so one metric is single-tier and one is not.
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

	// The reference half of the metrics doc: every metric described somewhere
	// outside the ledger.
	goodReference = "# Metrics\n\n" +
		"| Metric | Description |\n| --- | --- |\n" +
		"| `actions_gateway_jobs_acquired_total` | Jobs acquired. |\n" +
		"| `actions_gateway_worker_pods_reaped_total` | Pods reaped. |\n" +
		"| `actions_gateway_scaleset_jobs_assigned_total` | Jobs assigned. |\n" +
		"| `actions_gateway_worker_quota_pressure` | Quota pressure. |\n" +
		"| `actions_gateway_eviction_retries_total` | Recoveries started. |\n\n"

	goodLedger = "## Acquisition-tier reach\n\n" +
		"| Metric | Tier | Why |\n| --- | --- | --- |\n" +
		"| `actions_gateway_jobs_acquired_total` | Classic only | No acquirejob on the other tier. |\n" +
		"| `actions_gateway_worker_pods_reaped_total` | Both | The reaper is protocol-agnostic. |\n" +
		"| `actions_gateway_scaleset_jobs_assigned_total` | Scale-set only | The queue's own delivery signal. |\n" +
		"| `actions_gateway_worker_quota_pressure` | Classic only | A v1 collector. |\n" +
		"| `actions_gateway_eviction_retries_total` | Both | Ported, split by the tier label. |\n\n" +
		goodValues +
		"## Next\n"

	// One derived row, whose tier the scale-set-only file proves, and one the
	// file layout cannot prove, which therefore cites the guard instead.
	goodValues = "### Label-value reach\n\n" +
		"| Metric | Label | Value | Tier | Why |\n| --- | --- | --- | --- | --- |\n" +
		"| `actions_gateway_eviction_retries_total` | `cause` | `vanished` | Scale-set only | Only the orphan scan produces it. |\n" +
		"| `actions_gateway_eviction_retries_total` | `cause` | `abandoned` | Classic only | The renew loop is the only producer (`internal/provisioner/eviction.go`). |\n\n"

	goodParity = "### Capability parity\n\n" +
		"**Correctly absent from the scale-set tier** are `jobs_acquired_total` and " +
		"`worker_quota_pressure`, artifacts of the many-acquirers model.\n\n" +
		"More prose.\n"
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

// runCase writes all three inputs and returns the findings.
func runCase(t *testing.T, srcDir, metricsDoc, parityDoc string) []string {
	t.Helper()
	dir := t.TempDir()
	mPath := filepath.Join(dir, "observability-metrics.md")
	pPath := filepath.Join(dir, "v2-ga.md")
	writeFile(t, mPath, metricsDoc)
	writeFile(t, pPath, parityDoc)
	findings, err := run(srcDir, mPath, pPath)
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
	findings := runCase(t, tree(t, nil), goodReference+goodLedger, goodParity)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// The case Q776 exists for: a metric reaches one tier and the ledger never
// records it, which is how Q683, Q691, Q713 and Q844 each shipped.
func TestMetricMissingFromLedgerFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `actions_gateway_jobs_acquired_total` | Classic only | No acquirejob on the other tier. |\n", "", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "actions_gateway_jobs_acquired_total is defined in")
	requireFinding(t, findings, "which acquisition tier emits it")
}

func TestLedgerRowWithoutAMetricFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `actions_gateway_eviction_retries_total` | Both | Ported, split by the tier label. |\n",
		"| `actions_gateway_eviction_retries_total` | Both | Ported, split by the tier label. |\n"+
			"| `actions_gateway_gone_total` | Both | Removed last release. |\n", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "actions_gateway_gone_total is listed in")
}

// The stale-after-a-port direction: the source emits from the tier the ledger
// says is excluded.
func TestClassicOnlyClaimRefutedByAScaleSetSiteFails(t *testing.T) {
	port := `package provisioner

func (p *P) recover() {
	p.Metrics.JobsAcquired.WithLabelValues(p.ns).Inc()
}
`
	src := tree(t, map[string]string{"internal/provisioner/eviction_scaleset.go": port})
	findings := runCase(t, src, goodReference+goodLedger, goodParity)
	requireFinding(t, findings, "eviction_scaleset.go: actions_gateway_jobs_acquired_total is emitted here")
}

func TestScaleSetOnlyClaimRefutedByAClassicSiteFails(t *testing.T) {
	classic := listenerSrc + `
func (l *L) also() {
	l.ScaleSetMetrics.Assigned.WithLabelValues(l.ns).Inc()
}
`
	src := tree(t, map[string]string{"internal/listener/goroutine.go": classic})
	findings := runCase(t, src, goodReference+goodLedger, goodParity)
	requireFinding(t, findings, "actions_gateway_scaleset_jobs_assigned_total is emitted here")
}

// A registered collector nothing writes through publishes a permanent zero,
// which an operator cannot tell from a quiet system.
func TestRegisteredButNeverEmittedFails(t *testing.T) {
	src := tree(t, map[string]string{"internal/controller/runner_shared.go": "package controller\n"})
	findings := runCase(t, src, goodReference+goodLedger, goodParity)
	requireFinding(t, findings, "actions_gateway_worker_pods_reaped_total (field PodsReaped) is registered and never emitted")
}

func TestSingleTierRowWithoutAReasonFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `actions_gateway_jobs_acquired_total` | Classic only | No acquirejob on the other tier. |",
		"| `actions_gateway_jobs_acquired_total` | Classic only |  |", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "with no reason")
}

func TestUnknownTierValueFails(t *testing.T) {
	ledger := strings.Replace(goodLedger, "| Both | The reaper is protocol-agnostic. |",
		"| Mostly | The reaper is protocol-agnostic. |", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, `has tier "Mostly"`)
}

// A metric documented by its tier and nothing else gives an operator no way to
// read it. eviction_recovery_evidence_lost_total shipped that way (Q809).
func TestMetricOnlyInTheLedgerFails(t *testing.T) {
	reference := strings.Replace(goodReference,
		"| `actions_gateway_worker_quota_pressure` | Quota pressure. |\n", "", 1)
	findings := runCase(t, tree(t, nil), reference+goodLedger, goodParity)
	requireFinding(t, findings, "is in the tier ledger and nowhere else")
}

func TestAbsentByDesignListDisagreeingWithTheLedgerFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `actions_gateway_jobs_acquired_total` | Classic only | No acquirejob on the other tier. |",
		"| `actions_gateway_jobs_acquired_total` | Both | Ported. |", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, `named absent-by-design on the scale-set tier, and the ledger calls it "Both"`)
}

func TestMissingAbsentByDesignParagraphFails(t *testing.T) {
	findings := runCase(t, tree(t, nil), goodReference+goodLedger, "### Capability parity\n\nNothing here.\n")
	requireFinding(t, findings, "absent-by-design list gates the classic removal")
}

// The ledger is the gate's only input for the tier question, so its absence must
// be an error rather than a green run over zero rows.
func TestMissingLedgerSectionIsAnError(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "observability-metrics.md")
	pPath := filepath.Join(dir, "v2-ga.md")
	writeFile(t, mPath, goodReference)
	writeFile(t, pPath, goodParity)
	if _, err := run(tree(t, nil), mPath, pPath); err == nil {
		t.Fatal("expected an error when the ledger section is absent")
	}
}

// A scan that matched nothing looks exactly like a clean tree, so an empty
// inventory must refuse rather than report green.
func TestEmptySourceTreeIsAnError(t *testing.T) {
	dir := t.TempDir()
	mPath := filepath.Join(dir, "observability-metrics.md")
	pPath := filepath.Join(dir, "v2-ga.md")
	writeFile(t, mPath, goodReference+goodLedger)
	writeFile(t, pPath, goodParity)
	if _, err := run(t.TempDir(), mPath, pPath); err == nil {
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
	findings := runCase(t, src, goodReference+goodLedger, goodParity)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// The Q851 case: a series populates on both tiers while one of its label values
// reaches only one, which every series-level check reads as healthy.
func TestSingleTierValueWithoutARowFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `actions_gateway_eviction_retries_total` | `cause` | `vanished` | Scale-set only | Only the orphan scan produces it. |\n", "", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, `actions_gateway_eviction_retries_total{cause="vanished"}`)
	requireFinding(t, findings, "give it a \"### Label-value reach\" row")
}

// The other direction, matching the series-level contradiction check: the row
// names a tier, and the file naming the value is the other tier's.
func TestValueRowTheSourceRefutesFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `actions_gateway_eviction_retries_total` | `cause` | `vanished` | Scale-set only | Only the orphan scan produces it. |",
		"| `actions_gateway_eviction_retries_total` | `cause` | `vanished` | Classic only | Seeded defect. |", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "is named here, and the ledger calls it \"Classic only\"")
}

// A claim the file layout cannot prove rests on a guard, and the row has to point
// at it — otherwise the value table is the hand-kept list the ledger replaced.
func TestUnprovableValueRowWithoutACitationFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"The renew loop is the only producer (`internal/provisioner/eviction.go`).",
		"The renew loop is the only producer.", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "cite the .go file whose guard does")
}

func TestValueRowCitingAMissingFileFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"`internal/provisioner/eviction.go`", "`internal/provisioner/gone.go`", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "which is not a file the AGC scan parsed")
}

// The vocabulary is closed for the same reason the series table's is: a value row
// records an exception, so "Both" is not an answer it can carry.
func TestValueRowWithANonExceptionTierFails(t *testing.T) {
	ledger := strings.Replace(goodLedger, "| `vanished` | Scale-set only |", "| `vanished` | Both |", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "a value row records an exception")
}

func TestValueRowNamingAnAbsentLabelFails(t *testing.T) {
	ledger := strings.Replace(goodLedger,
		"| `actions_gateway_eviction_retries_total` | `cause` | `vanished` |",
		"| `actions_gateway_eviction_retries_total` | `outcome` | `vanished` |", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, `has no "outcome" label`)
}

func TestValueRowNamingAnUnemittedValueFails(t *testing.T) {
	ledger := strings.Replace(goodLedger, "| `vanished` | Scale-set only |", "| `evaporated` | Scale-set only |", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "no site in the AGC source names that value")
}

// The tier label is the axis, not a value on it, so a row about it would restate
// the series row and go stale on its own schedule.
func TestValueRowOnTheTierLabelFails(t *testing.T) {
	ledger := strings.Replace(goodLedger, "## Next\n",
		"| `actions_gateway_eviction_retries_total` | `tier` | `scaleset` | Scale-set only | Tautology. |\n\n## Next\n", 1)
	findings := runCase(t, tree(t, nil), goodReference+ledger, goodParity)
	requireFinding(t, findings, "states the acquisition tier as its own value")
}

// Help is what an operator reads off /metrics with no docs open, so a vocabulary
// the code has outgrown reads there as a value that cannot occur.
func TestHelpMissingAnEmittedValueFails(t *testing.T) {
	src := tree(t, map[string]string{
		"internal/runnercore/metrics.go": strings.Replace(coreSrc,
			"h, by cause (eviction, vanished, abandoned).", "h, by cause (eviction, abandoned).", 1),
	})
	findings := runCase(t, src, goodReference+goodLedger, goodParity)
	requireFinding(t, findings, `emits cause="vanished"`)
	requireFinding(t, findings, "its Help does not name it")
}

// The seam that sets the resolution bound: the scale-set arm reaches a shared
// counter through a wrapper and a classifier, so a derivation that stops early
// sees the classic literals alone and calls every value classic-only.
func TestValueReachedThroughAWrapperAndClassifierIsNotSingleTier(t *testing.T) {
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
	findings := runCase(t, src, goodReference+goodLedger, goodParity)
	for _, f := range findings {
		if strings.Contains(f, `cause="eviction"`) {
			t.Fatalf("eviction reaches both tiers and must not be called single-tier: %s", f)
		}
	}
}
