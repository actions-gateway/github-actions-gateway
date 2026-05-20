// Package controller implements the RunnerGroup reconciler.
//
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.github.com,resources=runnergroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
package controller
