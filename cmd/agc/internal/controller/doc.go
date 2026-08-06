// Package controller implements the RunnerGroup reconciler.
//
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;patch;delete
// configmaps: the per-RunnerSet guard ConfigMap persisting a scale-set listener's
// concluded-job state across a hard kill (Q606). get/create/update only — reads are
// uncached (no informer, so no list/watch), and the ConfigMap is owner-ref'd to its
// RunnerSet, so deletion is the garbage collector's.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update
// pods patch: metadata-only annotation stamps — the reaper's deletion-reason mark
// (Q502), the scale-set eviction-recovery claim, and the job-completed-at reap
// deadline. Never a spec or status write.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
// events: both the core ("") and events.k8s.io grants are required — the
// new-style recorder (mgr.GetEventRecorder) writes events.k8s.io/v1 Events.
// get/list on the core group additionally serve the capacity gate's
// gate on a cluster that can grow (Q406), which reads a stuck worker pod's Events to learn
// whether the cluster autoscaler declined to add a node for it. Read-only, and
// core-only because the two groups serve one underlying store; watch is
// deliberately withheld — the reads are field-selected to a single pod and there is
// no Event informer, which is the whole point of doing them uncached.
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// resourcequotas read-only: the RunnerGroup reconciler reads the namespace
// ResourceQuota to compute the WorkerQuota{Pressure,Exceeded} conditions (Q82).
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
// pods.metrics.k8s.io read-only: the worker usage sampler lists PodMetrics to
// aggregate per-RunnerSet CPU/memory peaks for right-sizing (Q359). PodMetrics
// are namespaced, so the per-tenant RoleBinding scopes this to the tenant
// namespace; the sampler degrades gracefully when metrics-server is absent.
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// runtimeclasses read-only (Q450): a worker pod's ResourceQuota charge includes its
// RuntimeClass pod overhead (250m/160Mi on the reference Kata shape), and overhead
// lives on the cluster-scoped RuntimeClass, not the pod template. Without this the
// WorkerQuota conditions, the pre-claim gate, and the scale-set capacity integer all
// under-count a Kata worker. Read-only on a kind that carries no tenant data; the
// read is fail-open, so an install that has not yet been granted it behaves exactly
// as it did before Q450. Cluster-scoped, so it is bound by the per-gateway
// agc-clusterrunnertemplate-reader ClusterRoleBinding, not the tenant RoleBinding.
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list;watch
//
// v2 (actions-gateway.com): the RunnerSet reconciler reconciles RunnerSets and
// reads their references (gatewayRef → ActionsGateway, proxyRef/defaultProxyRef →
// EgressProxy, templateRef → RunnerTemplate/ClusterRunnerTemplate) at runtime to
// resolve the worker pod shape and egress proxy (§H.7). These markers mirror the
// chart's hand-maintained agc-tenant-role and agc-clusterrunnertemplate-reader
// grants (charts/actions-gateway/files/agc-*-rules.yaml) so the generated agc-role
// no longer drifts from the shipped v2 permission set. That pairing is gated by
// rbac_chart_drift_test.go (Q454): every marker here must be shipped by one of the
// two chart roles, and the cluster-scoped reader's rules must match these verbs
// exactly — so a marker added without its chart rule fails the build, not a
// production install.
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways;egressproxies;runnertemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=clusterrunnertemplates,verbs=get;list;watch
package controller
