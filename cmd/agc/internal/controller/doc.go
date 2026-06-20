// Package controller implements the RunnerGroup reconciler.
//
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// resourcequotas read-only: the RunnerGroup reconciler reads the namespace
// ResourceQuota to compute the WorkerQuota{Pressure,Exceeded} conditions (Q82).
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
package controller
