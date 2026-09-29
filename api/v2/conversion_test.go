package v2_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

func ptrTo[T any](v T) *T { return &v }

// fixedTime is a whole-second timestamp so metav1.Time survives the JSON round-trip.
var fixedTime = metav1.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)

// assertDeepEqual fails with a readable JSON diff when want != got, comparing with
// k8s semantic equality so metav1.Time compares by instant.
func assertDeepEqual(t *testing.T, what string, want, got any) {
	t.Helper()
	if apiequality.Semantic.DeepEqual(want, got) {
		return
	}
	w, _ := json.MarshalIndent(want, "", "  ")
	g, _ := json.MarshalIndent(got, "", "  ")
	t.Errorf("%s round-trip mismatch:\n--- want ---\n%s\n--- got ---\n%s", what, w, g)
}

// Every round-trip below starts from the hub, because v2beta1 is the storage
// version: the case that matters is a stored object read at v2 and written back.

func TestActionsGatewayConversion_RoundTrip(t *testing.T) {
	hub := &v2beta1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns", Labels: map[string]string{"k": "v"}},
		Spec: v2beta1.ActionsGatewaySpec{
			Credentials: v2beta1.GitHubCredentials{
				Type:      v2beta1.CredentialTypeGitHubApp,
				GitHubApp: &v2beta1.LocalSecretReference{Name: "app-secret"},
			},
			GitHubURL:          "https://github.com/my-org",
			DefaultProxyRef:    &v2beta1.ProxyObjectRef{Name: "proxy", Namespace: "platform"},
			DefaultTemplateRef: &v2beta1.ObjectRef{Name: "tmpl"},
			DefaultRunnerGroup: "tenant-a",
			AGCResources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
			LogLevel: "debug",
		},
		Status: v2beta1.ActionsGatewayStatus{
			Conditions:         []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: fixedTime}},
			ProxyMode:          "Proxied",
			ObservedGeneration: 3,
		},
	}
	want := hub.DeepCopy()
	var spoke v2.ActionsGateway
	if err := spoke.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v2beta1.ActionsGateway
	if err := spoke.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	assertDeepEqual(t, "ActionsGateway", want, &back)
}

// TestRunnerSetConversion_KeepsV2alpha1Annotations pins that a RunnerSet created at
// v2alpha1 keeps the conversion annotations carrying its acquisitionProtocol and
// maxListeners through a read and write-back at v2. Dropping them would silently
// re-protocol a Classic set the next time a v2 client updates it.
func TestRunnerSetConversion_KeepsV2alpha1Annotations(t *testing.T) {
	hub := &v2beta1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rs",
			Namespace: "tenant-a",
			Annotations: map[string]string{
				"conversion.actions-gateway.com/acquisition-protocol": "Classic",
				"conversion.actions-gateway.com/max-listeners":        "25",
			},
		},
		Spec: v2beta1.RunnerSetSpec{
			GatewayRef:         v2beta1.ObjectRef{Name: "gw"},
			TemplateRef:        &v2beta1.ObjectRef{Name: "tmpl", Kind: "RunnerTemplate"},
			ProxyRef:           &v2beta1.ProxyObjectRef{Name: "proxy", Namespace: "platform"},
			MaxWorkers:         ptrTo[int32](8),
			RunnerLabels:       []string{"linux", "x64"},
			PriorityTiers:      []v2beta1.PriorityTier{{PriorityClassName: "high", Threshold: 8}},
			EvictionRetryDelay: &metav1.Duration{Duration: 5 * time.Second},
			ScaleUp:            &v2beta1.ScaleUpRateLimit{MaxPerSecond: 5, Burst: ptrTo[int32](10)},
		},
		Status: v2beta1.RunnerSetStatus{
			ActiveJobs:         1,
			AdvertisedCapacity: ptrTo[int32](6),
			WithheldCapacity:   []v2beta1.WithheldCapacity{{Reason: "quota", Slots: 2}, {Reason: "capacity", Slots: 0}},
			ObservedGeneration: 7,
		},
	}
	want := hub.DeepCopy()
	var spoke v2.RunnerSet
	if err := spoke.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if got := spoke.Annotations["conversion.actions-gateway.com/acquisition-protocol"]; got != "Classic" {
		t.Errorf("v2 view dropped the acquisition-protocol annotation: got %q", got)
	}
	var back v2beta1.RunnerSet
	if err := spoke.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	assertDeepEqual(t, "RunnerSet", want, &back)
}

func TestRunnerTemplateConversion_RoundTrip(t *testing.T) {
	spec := v2beta1.RunnerTemplateSpec{
		WorkerImage: "ghcr.io/example/runner:latest",
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Image: "ghcr.io/example/runner:latest"}},
			},
		},
	}

	hubRT := &v2beta1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "ns"}, Spec: spec}
	wantRT := hubRT.DeepCopy()
	var spokeRT v2.RunnerTemplate
	if err := spokeRT.ConvertFrom(hubRT); err != nil {
		t.Fatalf("RunnerTemplate ConvertFrom: %v", err)
	}
	var backRT v2beta1.RunnerTemplate
	if err := spokeRT.ConvertTo(&backRT); err != nil {
		t.Fatalf("RunnerTemplate ConvertTo: %v", err)
	}
	assertDeepEqual(t, "RunnerTemplate", wantRT, &backRT)

	hubCRT := &v2beta1.ClusterRunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "golden"}, Spec: *spec.DeepCopy()}
	wantCRT := hubCRT.DeepCopy()
	var spokeCRT v2.ClusterRunnerTemplate
	if err := spokeCRT.ConvertFrom(hubCRT); err != nil {
		t.Fatalf("ClusterRunnerTemplate ConvertFrom: %v", err)
	}
	var backCRT v2beta1.ClusterRunnerTemplate
	if err := spokeCRT.ConvertTo(&backCRT); err != nil {
		t.Fatalf("ClusterRunnerTemplate ConvertTo: %v", err)
	}
	assertDeepEqual(t, "ClusterRunnerTemplate", wantCRT, &backCRT)
}

// TestEgressProxyConversion_ModeRoundTrip round-trips every egressPolicyMode v2
// defines, plus the empty value an object that skipped defaulting carries.
func TestEgressProxyConversion_ModeRoundTrip(t *testing.T) {
	for _, mode := range []v2beta1.EgressPolicyMode{v2beta1.EgressPolicyModeCIDR, v2beta1.EgressPolicyModeFQDN, ""} {
		t.Run(string(mode), func(t *testing.T) {
			hub := &v2beta1.EgressProxy{
				ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "ns"},
				Spec: v2beta1.EgressProxySpec{
					MinReplicas:      ptrTo[int32](1),
					MaxReplicas:      ptrTo[int32](3),
					EgressPolicyMode: mode,
					DestinationCIDRs: []string{"10.0.0.0/8"},
					LogLevel:         "debug",
					AuditLogging:     "Connections",
				},
				Status: v2beta1.EgressProxyStatus{
					Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: fixedTime}},
				},
			}
			if mode == v2beta1.EgressPolicyModeFQDN {
				hub.Spec.DestinationFQDNs = []string{"proxy.golang.org"}
			}
			want := hub.DeepCopy()
			var spoke v2.EgressProxy
			if err := spoke.ConvertFrom(hub); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			if got := string(spoke.Spec.EgressPolicyMode); got != string(mode) {
				t.Errorf("mode must survive hub->v2 verbatim: got %q, want %q", got, mode)
			}
			var back v2beta1.EgressProxy
			if err := spoke.ConvertTo(&back); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			assertDeepEqual(t, "EgressProxy egressPolicyMode="+string(mode), want, &back)
		})
	}
}

// annEgressPolicyMode mirrors the unexported conversion annotation in conversion.go.
const annEgressPolicyMode = "conversion.actions-gateway.com/egress-policy-mode"

// aliasHub is a stored EgressProxy naming a deprecated alias, beside an unrelated
// annotation the conversion must leave alone.
func aliasHub(mode v2beta1.EgressPolicyMode) *v2beta1.EgressProxy {
	return &v2beta1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "tenant-a", Annotations: map[string]string{"team": "infra"}},
		Spec: v2beta1.EgressProxySpec{
			EgressPolicyMode: mode,
			DestinationFQDNs: []string{"proxy.golang.org"},
		},
	}
}

// TestEgressProxyConversion_CarriesAlias pins that a stored alias reads at v2 as FQDN
// with the alias in the conversion annotation, and that writing the view back
// unchanged restores the stored object exactly.
func TestEgressProxyConversion_CarriesAlias(t *testing.T) {
	for _, mode := range []v2beta1.EgressPolicyMode{v2beta1.EgressPolicyModeCiliumFQDN, v2beta1.EgressPolicyModeCalicoFQDN} {
		t.Run(string(mode), func(t *testing.T) {
			hub := aliasHub(mode)
			want := hub.DeepCopy()
			var spoke v2.EgressProxy
			if err := spoke.ConvertFrom(hub); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			if spoke.Spec.EgressPolicyMode != v2.EgressPolicyModeFQDN {
				t.Errorf("v2 view mode = %q, want FQDN", spoke.Spec.EgressPolicyMode)
			}
			if got := spoke.Annotations[annEgressPolicyMode]; got != string(mode) {
				t.Errorf("v2 view annotation %s = %q, want %q", annEgressPolicyMode, got, mode)
			}
			var back v2beta1.EgressProxy
			if err := spoke.ConvertTo(&back); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			assertDeepEqual(t, "EgressProxy alias "+string(mode), want, &back)
		})
	}
}

// TestEgressProxyConversion_AliasDroppedByV2Edit pins the two v2 edits that move a
// pool off its alias: setting another mode, and deleting the annotation.
func TestEgressProxyConversion_AliasDroppedByV2Edit(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*v2.EgressProxy)
		want v2beta1.EgressPolicyMode
	}{
		"mode set to CIDR": {
			edit: func(p *v2.EgressProxy) {
				p.Spec.EgressPolicyMode = v2.EgressPolicyModeCIDR
				p.Spec.DestinationFQDNs = nil
			},
			want: v2beta1.EgressPolicyModeCIDR,
		},
		"annotation deleted": {
			edit: func(p *v2.EgressProxy) { delete(p.Annotations, annEgressPolicyMode) },
			want: v2beta1.EgressPolicyModeFQDN,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var spoke v2.EgressProxy
			if err := spoke.ConvertFrom(aliasHub(v2beta1.EgressPolicyModeCalicoFQDN)); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			tc.edit(&spoke)
			var back v2beta1.EgressProxy
			if err := spoke.ConvertTo(&back); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			if back.Spec.EgressPolicyMode != tc.want {
				t.Errorf("stored mode = %q, want %q", back.Spec.EgressPolicyMode, tc.want)
			}
			if _, ok := back.Annotations[annEgressPolicyMode]; ok {
				t.Errorf("conversion annotation reached the hub: %v", back.Annotations)
			}
			if back.Annotations["team"] != "infra" {
				t.Errorf("unrelated annotation lost: %v", back.Annotations)
			}
		})
	}
}

// TestEgressProxyConversion_RejectsUnknownAnnotation pins that a v2 write carrying a
// conversion annotation that names no alias fails rather than being ignored.
func TestEgressProxyConversion_RejectsUnknownAnnotation(t *testing.T) {
	spoke := v2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "tenant-a", Annotations: map[string]string{annEgressPolicyMode: "FQDN"}},
		Spec:       v2.EgressProxySpec{EgressPolicyMode: v2.EgressPolicyModeFQDN},
	}
	var back v2beta1.EgressProxy
	err := spoke.ConvertTo(&back)
	if err == nil || !strings.Contains(err.Error(), annEgressPolicyMode) {
		t.Fatalf("ConvertTo error = %v, want one naming %s", err, annEgressPolicyMode)
	}
}

// TestEgressProxyShapeMatchesHub pins v2.EgressProxy's JSON shape to the hub's.
// egressproxy_types.go is the one converted-kind file check-v2-api-sync.sh cannot hold
// byte-identical across v2beta1 and v2 (the enum differs), and the JSON round-trip
// drops a field present on one side only instead of failing, so a one-sided field
// edit there would otherwise lose data silently.
func TestEgressProxyShapeMatchesHub(t *testing.T) {
	if diff := shapeDiff("EgressProxy", reflect.TypeOf(v2.EgressProxy{}), reflect.TypeOf(v2beta1.EgressProxy{})); diff != "" {
		t.Fatal(diff)
	}
}

// TestShapeDiff_DetectsDivergence proves shapeDiff can fail: a renamed json tag and a
// changed field type both have to be reported.
func TestShapeDiff_DetectsDivergence(t *testing.T) {
	type a struct {
		X int    `json:"x"`
		Y string `json:"y"`
	}
	type renamed struct {
		X int    `json:"x"`
		Y string `json:"why"`
	}
	type retyped struct {
		X int64  `json:"x"`
		Y string `json:"y"`
	}
	if shapeDiff("t", reflect.TypeOf(a{}), reflect.TypeOf(renamed{})) == "" {
		t.Error("shapeDiff missed a renamed json tag")
	}
	if shapeDiff("t", reflect.TypeOf(a{}), reflect.TypeOf(retyped{})) == "" {
		t.Error("shapeDiff missed a changed field type")
	}
	if d := shapeDiff("t", reflect.TypeOf(a{}), reflect.TypeOf(a{})); d != "" {
		t.Errorf("shapeDiff reported identical types as divergent: %s", d)
	}
}

// shapeDiff reports the first difference between two types' JSON shapes. Types from
// a third package must be the same type; types from the two API versions are walked
// field by field, since they are distinct Go types by construction.
func shapeDiff(path string, a, b reflect.Type) string {
	if a == b {
		return ""
	}
	if a.Kind() != b.Kind() {
		return path + ": kind " + a.Kind().String() + " vs " + b.Kind().String()
	}
	if a.Name() != b.Name() {
		return path + ": type " + a.String() + " vs " + b.String()
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return shapeDiff(path+"[]", a.Elem(), b.Elem())
	case reflect.Map:
		if d := shapeDiff(path+"{key}", a.Key(), b.Key()); d != "" {
			return d
		}
		return shapeDiff(path+"{}", a.Elem(), b.Elem())
	case reflect.Struct:
		if a.NumField() != b.NumField() {
			return path + ": field count differs"
		}
		for i := range a.NumField() {
			fa, fb := a.Field(i), b.Field(i)
			if fa.Name != fb.Name || fa.Tag.Get("json") != fb.Tag.Get("json") {
				return path + ": field " + fa.Name + " `" + fa.Tag.Get("json") + "` vs " + fb.Name + " `" + fb.Tag.Get("json") + "`"
			}
			if d := shapeDiff(path+"."+fa.Name, fa.Type, fb.Type); d != "" {
				return d
			}
		}
		return ""
	default:
		// Two distinct named scalars (the per-version EgressPolicyMode, say) of the
		// same kind and name serialise identically.
		return ""
	}
}
