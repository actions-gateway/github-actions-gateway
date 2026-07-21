// Package controller implements the RunnerGroup reconciler.
//
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
// events: both the core ("") and events.k8s.io grants are required — the
// new-style recorder (mgr.GetEventRecorder) writes events.k8s.io/v1 Events.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// resourcequotas read-only: the RunnerGroup reconciler reads the namespace
// ResourceQuota to compute the WorkerQuota{Pressure,Exceeded} conditions (Q82).
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
// pods.metrics.k8s.io read-only: the worker usage sampler lists PodMetrics to
// aggregate per-RunnerSet CPU/memory peaks for right-sizing (Q359). PodMetrics
// are namespaced, so the per-tenant RoleBinding scopes this to the tenant
// namespace; the sampler degrades gracefully when metrics-server is absent.
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
//
// v2 (actions-gateway.com): the RunnerSet reconciler reconciles RunnerSets and
// reads their references (gatewayRef → ActionsGateway, proxyRef/defaultProxyRef →
// EgressProxy, templateRef → RunnerTemplate/ClusterRunnerTemplate) at runtime to
// resolve the worker pod shape and egress proxy (§H.7). These markers mirror the
// chart's hand-maintained agc-tenant-role and agc-clusterrunnertemplate-reader
// grants (charts/actions-gateway/files/agc-*-rules.yaml) so the generated agc-role
// no longer drifts from the shipped v2 permission set.
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways;egressproxies;runnertemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=clusterrunnertemplates,verbs=get;list;watch
package controller
