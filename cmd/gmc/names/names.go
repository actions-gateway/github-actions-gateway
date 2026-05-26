// Package names provides the canonical on-cluster resource names for
// GMC-managed resources. Tests import these instead of hardcoding strings so
// that a single rename propagates everywhere.
package names

// ProxyName is the name of the proxy Deployment, Service, HPA, PDB, and the
// proxy NetworkPolicy in each tenant namespace.
const ProxyName = "actions-gateway-proxy"

// WorkloadNetworkPolicyName is the NetworkPolicy that restricts AGC and worker
// pod egress to the proxy Service only.
const WorkloadNetworkPolicyName = "actions-gateway-workload"
