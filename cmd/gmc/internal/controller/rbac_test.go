package controller

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func loadClusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	data, err := os.ReadFile("../../config/rbac/role.yaml")
	require.NoError(t, err, "config/rbac/role.yaml must exist; run 'make manifests'")
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))
	return &role
}

func TestClusterRole_NoWildcardVerbs(t *testing.T) {
	role := loadClusterRole(t)
	for _, rule := range role.Rules {
		for _, verb := range rule.Verbs {
			require.NotEqual(t, "*", verb, "wildcard verb found in ClusterRole rule: %v", rule)
		}
	}
}

func TestClusterRole_NoWildcardOnSensitiveResources(t *testing.T) {
	role := loadClusterRole(t)
	sensitive := map[string]bool{"secrets": true, "pods": true, "nodes": true}
	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if sensitive[resource] {
				for _, verb := range rule.Verbs {
					require.NotEqual(t, "*", verb,
						"wildcard verb on sensitive resource %q in ClusterRole rule: %v", resource, rule)
				}
			}
		}
	}
}
