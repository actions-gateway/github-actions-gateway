package controller

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Small building blocks with no ActionsGateway/EgressProxy API version in their
// signatures. Kept in a version-neutral file so the v1 sunset (Q273) removes only
// the v1-typed builders around them.

func ptr[T any](v T) *T { return &v }

// fieldRef returns the downward-API EnvVarSource for a pod field path (e.g.
// metadata.namespace).
func fieldRef(path string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}
}

// toStringMapIface converts a map[string]string to the map[string]interface{}
// shape unstructured nested content requires.
func toStringMapIface(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// formatResourceAttributes renders a resource-attribute map as the
// comma-separated key=value list OTEL_RESOURCE_ATTRIBUTES expects. Keys are
// sorted so the rendered value is deterministic — without this the random map
// iteration order would churn the AGC Deployment on every reconcile. Shared by the
// v1 and v2 tracing env builders.
func formatResourceAttributes(attrs map[string]string) string {
	if len(attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+attrs[k])
	}
	return strings.Join(pairs, ",")
}
