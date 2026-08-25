package v2alpha1_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

func mustQuantity(s string) resource.Quantity { return resource.MustParse(s) }

// The conversion annotation keys are pinned here as literals (not imported from the
// unexported package consts) so the test also guards the on-the-wire key format that
// a stored hub object carries during coexistence.
const (
	annAcquisitionProtocol = "conversion.actions-gateway.com/acquisition-protocol"
	annMaxListeners        = "conversion.actions-gateway.com/max-listeners"
)

func ptrTo[T any](v T) *T { return &v }

// fixedTime is a whole-second timestamp so metav1.Time survives the JSON round-trip
// (RFC3339 second precision) and reflect.DeepEqual stays exact.
var fixedTime = metav1.Date(2026, time.July, 6, 12, 0, 0, 0, time.UTC)

// assertDeepEqual fails with a readable JSON diff when want != got. It uses k8s
// semantic equality so metav1.Time compares by .Equal() (a JSON round-trip changes
// time.Time's unexported representation without changing the instant), not by
// reflect.DeepEqual.
func assertDeepEqual(t *testing.T, what string, want, got any) {
	t.Helper()
	if apiequality.Semantic.DeepEqual(want, got) {
		return
	}
	w, _ := json.MarshalIndent(want, "", "  ")
	g, _ := json.MarshalIndent(got, "", "  ")
	t.Errorf("%s round-trip mismatch:\n--- want ---\n%s\n--- got ---\n%s", what, w, g)
}

// fullRunnerSet builds a v2alpha1 RunnerSet with every optional field populated, so a
// round-trip exercises the whole spec, not just the protocol fields.
func fullRunnerSet(protocol string, labels ...string) *v2alpha1.RunnerSet {
	return &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "rs-example",
			Namespace:   "tenant-a",
			Labels:      map[string]string{"team": "platform"},
			Annotations: map[string]string{"user.example.com/note": "keep me"},
		},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:  v2alpha1.ObjectRef{Name: "gw"},
			TemplateRef: &v2alpha1.ObjectRef{Name: "tmpl", Kind: "RunnerTemplate"},
			// Namespace set so the round-trip covers the cross-namespace half of the
			// reference (§H.9); a conversion that dropped it would silently downgrade a
			// shared proxy to a same-namespace one.
			ProxyRef:            &v2alpha1.ProxyObjectRef{Name: "proxy", Namespace: "platform"},
			MaxListeners:        25,
			MaxWorkers:          ptrTo[int32](8),
			RunnerLabels:        labels,
			RunnerGroup:         "tenant-a",
			AcquisitionProtocol: protocol,
			PriorityTiers:       []v2alpha1.PriorityTier{{PriorityClassName: "high", Threshold: 8}},
			MaxEvictionRetries:  ptrTo[int32](3),
			EvictionRetryDelay:  &metav1.Duration{Duration: 5 * time.Second},
			MaxQuotaRetries:     ptrTo[int32](4),
			QuotaRetryDelay:     &metav1.Duration{Duration: 30 * time.Second},
			CompletedPodTTL:     &metav1.Duration{Duration: 5 * time.Minute},
			PendingPodDeadline:  &metav1.Duration{Duration: 10 * time.Minute},
			ScaleUp:             &v2alpha1.ScaleUpRateLimit{MaxPerSecond: 5, Burst: ptrTo[int32](10)},
		},
		Status: v2alpha1.RunnerSetStatus{
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Reconciled",
				Message:            "all good",
				LastTransitionTime: fixedTime,
			}},
			ActiveSessions: 2,
			ActiveJobs:     1,
			PendingJobs:    0,
			ProxyMode:      "Proxied",
			TemplateSource: "TemplateRef",
			// Q721's capacity accounting. The Slots: 0 entry is deliberate: the
			// advertisement publishes an explicit zero for every rung it evaluated, so
			// the round-trip has to carry that zero and not just a non-zero.
			// Q792's self-reported runner version, here for the same reason as the
			// capacity fields: a status field on one version only is dropped silently
			// by the JSON round-trip, and v2-api-sync-check exempts this file.
			ObservedRunnerVersion: "2.335.1",
			AdvertisedCapacity:    ptrTo[int32](6),
			WithheldCapacity: []v2alpha1.WithheldCapacity{
				{Reason: "quota", Slots: 2},
				{Reason: "capacity", Slots: 0},
			},
			// The sizing recommendation is carried here because the spoke↔hub
			// conversion is a JSON round-trip: a field whose json tag is renamed on
			// one version only is dropped silently rather than failing to compile.
			// Q485 renamed windowStart → windowStartTime across both versions; this
			// fixture is what makes the next such rename fail loudly.
			SizingProfileState: v2alpha1.SizingProfileStateActive,
			SizingRecommendation: []v2alpha1.ContainerSizingRecommendation{{
				Container:       "runner",
				Requests:        corev1.ResourceList{corev1.ResourceCPU: mustQuantity("500m"), corev1.ResourceMemory: mustQuantity("1Gi")},
				Limits:          corev1.ResourceList{corev1.ResourceMemory: mustQuantity("2Gi")},
				ObservedPeak:    corev1.ResourceList{corev1.ResourceCPU: mustQuantity("800m"), corev1.ResourceMemory: mustQuantity("1536Mi")},
				ObservedP95:     corev1.ResourceList{corev1.ResourceCPU: mustQuantity("450m"), corev1.ResourceMemory: mustQuantity("1Gi")},
				SampleCount:     25,
				WindowStartTime: fixedTime,
			}},
			ObservedGeneration: 7,
		},
	}
}

func TestRunnerSetConversion_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		labels   []string
	}{
		{"classic multi-label", v2alpha1.AcquisitionProtocolClassic, []string{"linux", "self-hosted"}},
		{"scaleset single-label", v2alpha1.AcquisitionProtocolScaleSet, []string{"linux"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := fullRunnerSet(tc.protocol, tc.labels...)
			want := orig.DeepCopy() // guard against the source being mutated

			// spoke -> hub
			var hub v2beta1.RunnerSet
			if err := orig.ConvertTo(&hub); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			// The source object must be untouched (deep copy, not aliased).
			assertDeepEqual(t, "source object", want, orig)
			// The two protocol fields ride across as annotations on the hub...
			if got := hub.Annotations[annAcquisitionProtocol]; got != tc.protocol {
				t.Errorf("hub %s = %q, want %q", annAcquisitionProtocol, got, tc.protocol)
			}
			if got := hub.Annotations[annMaxListeners]; got != "25" {
				t.Errorf("hub %s = %q, want %q", annMaxListeners, got, "25")
			}
			// ...and the user annotation is preserved alongside them.
			if got := hub.Annotations["user.example.com/note"]; got != "keep me" {
				t.Errorf("hub lost user annotation: got %q", got)
			}
			// The rest of the spec survives identically.
			if !reflect.DeepEqual(hub.Spec.RunnerLabels, tc.labels) {
				t.Errorf("hub runnerLabels = %v, want %v", hub.Spec.RunnerLabels, tc.labels)
			}

			// hub -> spoke
			var back v2alpha1.RunnerSet
			if err := back.ConvertFrom(&hub); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			// Conversion annotations are stripped from the surfaced v2alpha1 view.
			if _, ok := back.Annotations[annAcquisitionProtocol]; ok {
				t.Errorf("conversion annotation leaked into v2alpha1 view: %v", back.Annotations)
			}
			assertDeepEqual(t, "RunnerSet", want, &back)
		})
	}
}

// A v2beta1-native RunnerSet carries no conversion annotation, so ConvertFrom restores
// the ScaleSet defaults (acquisitionProtocol=ScaleSet, maxListeners=10) rather than
// leaving the fields zero.
func TestRunnerSetConversion_NativeHubDefaults(t *testing.T) {
	native := &v2beta1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs-native", Namespace: "tenant-b"},
		Spec: v2beta1.RunnerSetSpec{
			GatewayRef:   v2beta1.ObjectRef{Name: "gw"},
			RunnerLabels: []string{"linux"},
			MaxWorkers:   ptrTo[int32](4),
		},
	}
	var back v2alpha1.RunnerSet
	if err := back.ConvertFrom(native); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if back.Spec.AcquisitionProtocol != v2alpha1.AcquisitionProtocolScaleSet {
		t.Errorf("acquisitionProtocol = %q, want %q", back.Spec.AcquisitionProtocol, v2alpha1.AcquisitionProtocolScaleSet)
	}
	if back.Spec.MaxListeners != 10 {
		t.Errorf("maxListeners = %d, want 10", back.Spec.MaxListeners)
	}
	if back.Annotations != nil {
		t.Errorf("expected nil annotations on the restored view, got %v", back.Annotations)
	}
}

// A RunnerSet with no annotations at all must round-trip back to nil annotations, not
// an empty map left behind after the conversion annotations are deleted.
func TestRunnerSetConversion_NilAnnotationsRoundTrip(t *testing.T) {
	orig := &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns"},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:          v2alpha1.ObjectRef{Name: "gw"},
			RunnerLabels:        []string{"linux"},
			AcquisitionProtocol: v2alpha1.AcquisitionProtocolScaleSet,
			MaxListeners:        10,
		},
	}
	want := orig.DeepCopy()
	var hub v2beta1.RunnerSet
	if err := orig.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	var back v2alpha1.RunnerSet
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	assertDeepEqual(t, "RunnerSet (nil annotations)", want, &back)
}

// A hub carrying a malformed maxListeners annotation (only possible if hand-edited)
// surfaces as a conversion error rather than a silent zero.
func TestRunnerSetConversion_MalformedMaxListenersAnnotation(t *testing.T) {
	hub := &v2beta1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "rs",
			Namespace:   "ns",
			Annotations: map[string]string{annMaxListeners: "not-a-number"},
		},
		Spec: v2beta1.RunnerSetSpec{
			GatewayRef:   v2beta1.ObjectRef{Name: "gw"},
			RunnerLabels: []string{"linux"},
		},
	}
	var back v2alpha1.RunnerSet
	if err := back.ConvertFrom(hub); err == nil {
		t.Fatal("expected ConvertFrom to error on a non-integer maxListeners annotation")
	}
}

func TestActionsGatewayConversion_RoundTrip(t *testing.T) {
	orig := &v2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns", Labels: map[string]string{"k": "v"}},
		Spec: v2alpha1.ActionsGatewaySpec{
			Credentials: v2alpha1.GitHubCredentials{
				Type:      v2alpha1.CredentialTypeGitHubApp,
				GitHubApp: &v2alpha1.LocalSecretReference{Name: "app-secret"},
			},
			GitHubURL:          "https://github.com/my-org",
			DefaultProxyRef:    &v2alpha1.ProxyObjectRef{Name: "proxy"},
			DefaultTemplateRef: &v2alpha1.ObjectRef{Name: "tmpl"},
			DefaultRunnerGroup: "tenant-a",
			AGCResources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: mustQuantity("500m")},
			},
			LogLevel: "debug",
		},
		Status: v2alpha1.ActionsGatewayStatus{
			Conditions:         []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: fixedTime}},
			ProxyMode:          "Proxied",
			ObservedGeneration: 3,
		},
	}
	want := orig.DeepCopy()
	var hub v2beta1.ActionsGateway
	if err := orig.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	var back v2alpha1.ActionsGateway
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	assertDeepEqual(t, "ActionsGateway", want, &back)
}

func TestEgressProxyConversion_RoundTrip(t *testing.T) {
	orig := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "ns"},
		Spec: v2alpha1.EgressProxySpec{
			MinReplicas:      ptrTo[int32](1),
			MaxReplicas:      ptrTo[int32](3),
			DestinationFQDNs: []string{"example.com"},
			DestinationCIDRs: []string{"10.0.0.0/8"},
			LogLevel:         "debug",
		},
		Status: v2alpha1.EgressProxyStatus{
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: fixedTime}},
		},
	}
	want := orig.DeepCopy()
	var hub v2beta1.EgressProxy
	if err := orig.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	var back v2alpha1.EgressProxy
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	assertDeepEqual(t, "EgressProxy", want, &back)
}

// TestEgressProxyConversion_EgressPolicyModeRoundTrip fuzzes the v2beta1→v2alpha1→v2beta1
// round-trip over EVERY egressPolicyMode enum value, including the deprecated
// CiliumFQDN/CalicoFQDN aliases (Q245). It locks in the security-relevant invariant that
// the conversion carries the mode VERBATIM and never collapses the CNI-specific values to
// FQDN — so the Cilium-vs-Calico distinction of a stored object is never silently dropped
// when the enforcement backend moves to operator config (--fqdn-policy-backend).
func TestEgressProxyConversion_EgressPolicyModeRoundTrip(t *testing.T) {
	modes := []v2alpha1.EgressPolicyMode{
		v2alpha1.EgressPolicyModeCIDR,
		v2alpha1.EgressPolicyModeFQDN,
		v2alpha1.EgressPolicyModeCiliumFQDN,
		v2alpha1.EgressPolicyModeCalicoFQDN,
		"", // an object that skipped apiserver defaulting must also round-trip
	}
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			// Start from a v2beta1 (hub / storage) object, since the fuzz targets a stored
			// object surviving a read-as-v2alpha1 and write-back.
			hub := &v2beta1.EgressProxy{
				ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "ns"},
				Spec: v2beta1.EgressProxySpec{
					EgressPolicyMode: v2beta1.EgressPolicyMode(mode),
					DestinationFQDNs: fqdnsFor(mode),
				},
			}
			want := hub.DeepCopy()

			// hub -> spoke -> hub
			var spoke v2alpha1.EgressProxy
			if err := spoke.ConvertFrom(hub); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			if got := spoke.Spec.EgressPolicyMode; got != mode {
				t.Errorf("mode must survive hub->spoke verbatim (not collapsed to FQDN): got %q, want %q", got, mode)
			}

			var back v2beta1.EgressProxy
			if err := spoke.ConvertTo(&back); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			assertDeepEqual(t, "EgressProxy egressPolicyMode="+string(mode), want, &back)
		})
	}
}

// fqdnsFor returns a destinationFQDNs slice valid for the given mode: an FQDN-family mode
// may carry extra host suffixes (they exercise more of the spec), CIDR/empty may not.
func fqdnsFor(mode v2alpha1.EgressPolicyMode) []string {
	switch mode {
	case v2alpha1.EgressPolicyModeFQDN, v2alpha1.EgressPolicyModeCiliumFQDN, v2alpha1.EgressPolicyModeCalicoFQDN:
		return []string{"proxy.golang.org"}
	default:
		return nil
	}
}

func TestRunnerTemplateConversion_RoundTrip(t *testing.T) {
	origRT := &v2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "ns"},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: "ghcr.io/example/runner:latest",
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "ghcr.io/example/runner:latest"}},
				},
			},
		},
	}
	wantRT := origRT.DeepCopy()
	var hubRT v2beta1.RunnerTemplate
	if err := origRT.ConvertTo(&hubRT); err != nil {
		t.Fatalf("RunnerTemplate ConvertTo: %v", err)
	}
	var backRT v2alpha1.RunnerTemplate
	if err := backRT.ConvertFrom(&hubRT); err != nil {
		t.Fatalf("RunnerTemplate ConvertFrom: %v", err)
	}
	assertDeepEqual(t, "RunnerTemplate", wantRT, &backRT)

	origCRT := &v2alpha1.ClusterRunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "golden"},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: "ghcr.io/example/dind:latest",
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "ghcr.io/example/dind:latest"}}},
			},
		},
	}
	wantCRT := origCRT.DeepCopy()
	var hubCRT v2beta1.ClusterRunnerTemplate
	if err := origCRT.ConvertTo(&hubCRT); err != nil {
		t.Fatalf("ClusterRunnerTemplate ConvertTo: %v", err)
	}
	var backCRT v2alpha1.ClusterRunnerTemplate
	if err := backCRT.ConvertFrom(&hubCRT); err != nil {
		t.Fatalf("ClusterRunnerTemplate ConvertFrom: %v", err)
	}
	assertDeepEqual(t, "ClusterRunnerTemplate", wantCRT, &backCRT)
}
