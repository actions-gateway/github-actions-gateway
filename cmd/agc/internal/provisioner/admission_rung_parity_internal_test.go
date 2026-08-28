package provisioner

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The two-tier rung parity Q443 exists to enforce, as a gate rather than as the
// prose obligation in admission.go (Q973). Every rung is stated twice — once per
// delivered job in Admit, once per long-poll in AdvertiseCapacity — and a rung
// added to one but not the other ships to one acquisition tier, which is the
// defect Q443 fixed for the quota rung and Q717 then hand-mirrored for the rate
// rung.
//
// The obligation decays the way check-metric-tiers.sh records the same walk
// decaying four times before a gate replaced it. Two tests hold it here:
//
//	the case table below binds each rung in BOTH tiers and asserts what it produces
//	CoversEveryAdmitReason walks the AdmitReason* declarations and requires a case
//
// The second is what makes a new rung fail: adding a constant with no case is
// red before any of the wiring exists, so the obligation is met at the point the
// reason is coined rather than at review.

// parityTarget binds any single rung on either tier. The package's other two
// Target stubs are each one-sided — rungTarget answers the classic rungs and
// stubs the integer forms to (0, false), capacityTarget the reverse — and this
// walk needs one rig driven through both.
type parityTarget struct {
	ceiling        int32
	ceilingBounded bool

	quotaExhausted bool
	quotaLimit     int32
	quotaBounded   bool

	declined        bool
	declinedLimit   int32
	declinedBounded bool

	scaleUp *ScaleUpConfig
}

func (p *parityTarget) Key() client.ObjectKey             { return client.ObjectKey{Namespace: "ns", Name: "s"} }
func (p *parityTarget) OwnerRef() metav1.OwnerReference   { return metav1.OwnerReference{} }
func (p *parityTarget) PodOwnerLabels() map[string]string { return nil }
func (p *parityTarget) Ceiling(context.Context) (int32, bool) {
	return p.ceiling, p.ceilingBounded
}
func (p *parityTarget) QuotaExhausted(context.Context) (bool, string) {
	return p.quotaExhausted, "quota detail"
}
func (p *parityTarget) QuotaCapacity(context.Context, int32) (int32, bool) {
	return p.quotaLimit, p.quotaBounded
}
func (p *parityTarget) CapacityDeclined(context.Context) (bool, string) {
	return p.declined, "capacity detail"
}
func (p *parityTarget) DeclinedCapacity(context.Context, int32) (int32, bool) {
	return p.declinedLimit, p.declinedBounded
}
func (p *parityTarget) ScaleUpLimit(context.Context) *ScaleUpConfig { return p.scaleUp }
func (p *parityTarget) RecordEvent(_, _, _, _ string)               {}
func (p *parityTarget) Resolve(context.Context) (*ResolvedSpec, error) {
	return &ResolvedSpec{}, nil
}

// parityUnboundedDefault is the advertisement an owner with no declared ceiling
// and no rung binding would get, so every case's wantTotal being below it is what
// "this rung bound on the scale-set tier" means.
const parityUnboundedDefault = int32(10)

// admissionRungParityCase rigs exactly one rung and states what each tier must do
// with it.
type admissionRungParityCase struct {
	reason string
	target parityTarget
	// drainTokens is taken from the bucket before the classic-tier admit, for the
	// one rung that binds by having been spent rather than by a Target answer.
	drainTokens int
	// wantTotal is the advertisement the rung produces, always below
	// parityUnboundedDefault.
	wantTotal int32
	// withheld is whether the rung publishes its own Withheld entry. The ceiling is
	// the one rung that does not: the scale-set tier states it as the base the other
	// rungs withhold FROM, so it binds by lowering Total and has nothing to attribute.
	withheld bool
}

var admissionRungParityCases = []admissionRungParityCase{
	{
		reason:    runnercore.AdmitReasonCeiling,
		target:    parityTarget{ceilingBounded: true}, // a declared ceiling of zero
		wantTotal: 0,
		withheld:  false,
	},
	{
		reason:    runnercore.AdmitReasonQuota,
		target:    parityTarget{quotaExhausted: true, quotaLimit: 2, quotaBounded: true},
		wantTotal: 2,
		withheld:  true,
	},
	{
		reason:    runnercore.AdmitReasonCapacity,
		target:    parityTarget{declined: true, declinedLimit: 2, declinedBounded: true},
		wantTotal: 2,
		withheld:  true,
	},
	{
		reason:      runnercore.AdmitReasonScaleUp,
		target:      parityTarget{scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: 2}},
		drainTokens: 2,
		wantTotal:   2,
		withheld:    true,
	},
}

// TestAdmissionRungParity_EveryRungBindsInBothTiers drives each rung through both
// acquisition tiers with one rig. A rung wired into only one of them fails here
// on the tier it was never given.
func TestAdmissionRungParity_EveryRungBindsInBothTiers(t *testing.T) {
	ctx := context.Background()

	for _, tc := range admissionRungParityCases {
		t.Run(tc.reason, func(t *testing.T) {
			classic := tc.target
			p := newParityProvisioner()
			for range tc.drainTokens {
				require.True(t, p.scaleUp.allow(classic.Key().String(), classic.scaleUp),
					"draining the burst must succeed")
			}

			release, ok, reason := p.Admit(&classic)(ctx)

			assert.False(t, ok, "the rigged rung must refuse the claim")
			assert.Nil(t, release, "a refused job must carry no release func")
			assert.Equal(t, tc.reason, reason)

			scaleset := tc.target
			adv := newParityProvisioner().AdvertiseCapacity(&scaleset, parityUnboundedDefault)(ctx)

			assert.Equal(t, tc.wantTotal, adv.Total)
			assert.Less(t, adv.Total, parityUnboundedDefault,
				"the rung must lower the advertisement, not merely be evaluated")
			assert.LessOrEqual(t, adv.Total, adv.Ceiling,
				"the advertisement must never exceed the declared ceiling")
			if tc.withheld {
				assert.Positive(t, adv.Withheld[tc.reason],
					"a rung that lowers the total must attribute the slots it took")
			} else {
				assert.NotContains(t, adv.Withheld, tc.reason,
					"the ceiling is the advertisement's base, not a rung that withholds from it")
			}
		})
	}
}

// TestAdmissionRungParity_CoversEveryAdmitReason is the half that catches the rung
// nobody has written yet. The table above can only assert what it lists, so the
// AdmitReason* declarations are read from source and every one of them is required
// to have a case.
func TestAdmissionRungParity_CoversEveryAdmitReason(t *testing.T) {
	declared := declaredAdmitReasons(t)

	covered := map[string]bool{}
	for _, tc := range admissionRungParityCases {
		covered[tc.reason] = true
	}

	var uncovered, unknown []string
	for _, reason := range declared {
		if !covered[reason] {
			uncovered = append(uncovered, reason)
		}
	}
	for reason := range covered {
		if !slices.Contains(declared, reason) {
			unknown = append(unknown, reason)
		}
	}
	sort.Strings(uncovered)
	sort.Strings(unknown)

	if len(uncovered) == 0 && len(unknown) == 0 {
		return
	}
	t.Fatalf("the admission rung roster moved:\n%s%s\n%s",
		block("AdmitReason* constants with no parity case", uncovered),
		block("parity cases naming no AdmitReason* constant", unknown),
		"Every rung is stated twice — per delivered job in Admit, per long-poll in\n"+
			"AdvertiseCapacity — and one wired into a single tier ships to a single\n"+
			"acquisition tier (Q443). Add the rung to BOTH, then add a case to\n"+
			"admissionRungParityCases rigging it, so the pair is gated rather than\n"+
			"promised by a comment (Q973).")
}

// declaredAdmitReasons reads the AdmitReason* values from runnercore's source
// rather than from a second list here, so the roster cannot drift from the
// declarations it is a roster of. runnercore is in this module, so the read is a
// real test-cache input.
func declaredAdmitReasons(t *testing.T) []string {
	t.Helper()
	const src = "../runnercore/hooks.go"

	file, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	require.NoError(t, err)

	var reasons []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, "AdmitReason") || i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				require.True(t, ok,
					"%s is not a string literal; this walk reads the declarations directly", name.Name)
				unquoted, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				reasons = append(reasons, unquoted)
			}
		}
	}
	require.NotEmpty(t, reasons, "no AdmitReason* constant found in %s; the walk is measuring nothing", src)
	sort.Strings(reasons)
	return reasons
}

// newParityProvisioner is a provisioner whose rate rung is driveable: a fake clock
// so the bucket never refills mid-case, and a client the scale-set rung's
// in-flight pod count can list against.
func newParityProvisioner() *Provisioner {
	p := NewProvisioner(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build(), nil, nil)
	p.scaleUp = scaleUpLimiter{now: newFakeClock().now}
	return p
}
