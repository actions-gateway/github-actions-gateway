/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=actions-gateway.com,resources=actionsgateways/finalizers,verbs=update
// The AGC control-plane children (Deployment/SA/RoleBinding/Service/NetworkPolicy/
// Secret) write verbs are already granted to the GMC ClusterRole by the v1
// ActionsGateway reconciler markers, which controller-gen aggregates into the same
// manager-role; the v2 reconciler reuses them. It reads the namespace
// security-profile label (namespaces get;list;watch already granted) and the
// referenced EgressProxy (granted above).
//
// Multi-gateway (M3b): each gateway's AGC needs cluster-scoped read of the
// cluster-scoped ClusterRunnerTemplate kind, which a namespaced RoleBinding cannot
// grant. The GMC creates a per-gateway ClusterRoleBinding to the shipped
// agc-clusterrunnertemplate-reader ClusterRole; it holds clusterrolebindings CRUD
// and `bind` only on that exact ClusterRole name, so it never gains the read itself
// nor can bind AGC SAs into arbitrary ClusterRoles.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=agc-clusterrunnertemplate-reader

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ActionsGatewayV2Reconciler reconciles a v2alpha1 ActionsGateway into the
// per-tenant AGC control plane: the AGC Deployment/ServiceAccount/RoleBinding/
// Service, the AGC and workload NetworkPolicies, the metrics mTLS Secrets, and the
// AGC's egress wiring through the resolved EgressProxy. Every child carries a
// controller owner reference for clean cascade GC (§H.8).
//
// It is the v2 counterpart of the v1 ActionsGatewayReconciler, minus two
// responsibilities v2 moved elsewhere: the egress proxy pool is now a standalone
// EgressProxy (M2 reconciler) the gateway only references, and the namespace Pod
// Security Admission labels are stamped by the NamespacePSAReconciler from the
// namespace security-profile label (Q175) — this reconciler reads that label to
// thread SECURITY_PROFILE to the AGC but never stamps PSA. Single-gateway per
// namespace (M3a); proxy is required (no direct egress, §H.10).
type ActionsGatewayV2Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// AGCImage is the AGC container image injected into the Deployment.
	AGCImage string
	// AGCExtraEnv is the testing-only AGC_EXTRA_* passthrough (gated by the GMC's
	// --allow-agc-extra-env flag), forwarded verbatim to the AGC Deployment.
	AGCExtraEnv []corev1.EnvVar
	// APIServerCIDRs optionally scopes the AGC NetworkPolicy's apiserver egress
	// rule (Q145); empty keeps it any-destination (the secure default).
	APIServerCIDRs []string
	// Recorder emits Kubernetes Events on the ActionsGateway. May be nil in tests.
	Recorder events.EventRecorder
}

// Reconcile drives a v2 ActionsGateway toward its desired AGC control plane.
func (r *ActionsGatewayV2Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ag gmcv2alpha1.ActionsGateway
	if err := r.Get(ctx, req.NamespacedName, &ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ag.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ag)
	}

	if !controllerutil.ContainsFinalizer(&ag, gmcv2alpha1.ActionsGatewayFinalizer) {
		controllerutil.AddFinalizer(&ag, gmcv2alpha1.ActionsGatewayFinalizer)
		if err := r.Update(ctx, &ag); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Credential check: the AGC mounts the GitHub App Secret; without it, do not
	// provision (CredentialUnavailable, fail closed).
	var credSecret corev1.Secret
	credErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ag.Spec.GitHubAppRef.Name}, &credSecret)
	if credErr != nil && !apierrors.IsNotFound(credErr) {
		return ctrl.Result{}, credErr
	}
	if apierrors.IsNotFound(credErr) {
		return r.setNotReady(ctx, &ag, gmcv2alpha1.ConditionCredentialUnavailable, gmcv2alpha1.ReasonSecretNotFound,
			fmt.Sprintf("GitHub App Secret %q not found in namespace %q", ag.Spec.GitHubAppRef.Name, ag.Namespace))
	}

	// Resolve the control-plane egress proxy from defaultProxyRef. Proxy is
	// required in M3a: unset or missing ⇒ fail closed (ProxyNotFound), no AGC.
	if ag.Spec.DefaultProxyRef == nil {
		return r.setNotReady(ctx, &ag, gmcv2alpha1.ConditionDegraded, gmcv2alpha1.ReasonProxyNotFound,
			"ActionsGateway has no defaultProxyRef; an EgressProxy is required for control-plane egress")
	}
	var proxy gmcv2alpha1.EgressProxy
	proxyErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: ag.Spec.DefaultProxyRef.Name}, &proxy)
	if proxyErr != nil && !apierrors.IsNotFound(proxyErr) {
		return ctrl.Result{}, proxyErr
	}
	if apierrors.IsNotFound(proxyErr) {
		return r.setNotReady(ctx, &ag, gmcv2alpha1.ConditionDegraded, gmcv2alpha1.ReasonProxyNotFound,
			fmt.Sprintf("EgressProxy %q (defaultProxyRef) not found in namespace %q", ag.Spec.DefaultProxyRef.Name, ag.Namespace))
	}

	// Read the namespace's effective security profile (the source label the
	// NamespacePSAReconciler stamps PSA from) to thread SECURITY_PROFILE to the AGC.
	securityProfile, err := r.namespaceSecurityProfile(ctx, ag.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileResources(ctx, &ag, &proxy, securityProfile); err != nil {
		return r.setDegraded(ctx, &ag, err)
	}

	return r.updateStatus(ctx, &ag)
}

// reconcileResources creates or patches every AGC control-plane child, each
// owner-referenced to the ActionsGateway. Failures are wrapped with the failing
// step so setDegraded can name it (Q156).
func (r *ActionsGatewayV2Reconciler) reconcileResources(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, proxy *gmcv2alpha1.EgressProxy, securityProfile string) (retErr error) {
	log := logf.FromContext(ctx)
	var current string
	step := func(name string) { current = name; log.V(1).Info("reconcileResources step", "step", name) }
	defer func() {
		if retErr != nil {
			retErr = &provisioningError{step: current, err: retErr}
		}
	}()

	step("ServiceAccounts")
	if err := r.applyServiceAccount(ctx, ag, buildAGCServiceAccountV2(ag)); err != nil {
		return fmt.Errorf("AGC ServiceAccount: %w", err)
	}
	if err := r.applyServiceAccount(ctx, ag, buildWorkerServiceAccountV2(ag)); err != nil {
		return fmt.Errorf("worker ServiceAccount: %w", err)
	}

	step("AGC RoleBinding")
	if err := r.applyRoleBinding(ctx, ag, buildAGCRoleBindingV2(ag)); err != nil {
		return fmt.Errorf("AGC RoleBinding: %w", err)
	}

	step("ClusterRunnerTemplate ClusterRoleBinding")
	if err := r.applyClusterRunnerTemplateReaderBinding(ctx, ag); err != nil {
		return fmt.Errorf("ClusterRunnerTemplate ClusterRoleBinding: %w", err)
	}

	step("metrics TLS certs")
	if err := r.ensureMetricsCerts(ctx, ag); err != nil {
		return fmt.Errorf("metrics TLS certs: %w", err)
	}

	step("AGC Service")
	if err := r.applyService(ctx, ag, buildAGCServiceV2(ag)); err != nil {
		return fmt.Errorf("AGC Service: %w", err)
	}

	step("NetworkPolicies")
	if err := r.applyNetworkPolicy(ctx, ag, buildWorkloadNetworkPolicyV2(ag)); err != nil {
		return fmt.Errorf("workload NetworkPolicy: %w", err)
	}
	if err := r.applyNetworkPolicy(ctx, ag, buildAGCNetworkPolicyV2(ag, r.APIServerCIDRs)); err != nil {
		return fmt.Errorf("AGC NetworkPolicy: %w", err)
	}

	step("AGC Deployment")
	dep := buildAGCDeploymentV2(ag, r.AGCImage, proxy.Name, securityProfile, proxy.Spec.NoProxyCIDRs, r.AGCExtraEnv)
	if err := r.applyDeployment(ctx, ag, dep); err != nil {
		return fmt.Errorf("AGC Deployment: %w", err)
	}
	return nil
}

// namespaceSecurityProfile returns the effective Pod Security Admission profile
// for the tenant namespace, read from its security-profile label (baseline when
// absent). The reconciler only reads it; the NamespacePSAReconciler owns stamping
// the PSA labels (Q175).
func (r *ActionsGatewayV2Reconciler) namespaceSecurityProfile(ctx context.Context, namespace string) (string, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return "", fmt.Errorf("read namespace %q for security profile: %w", namespace, err)
	}
	return gmcv2alpha1.EffectiveSecurityProfile(ns.Labels[gmcv2alpha1.SecurityProfileLabel]), nil
}

// ensureMetricsCerts ensures the per-tenant metrics mTLS bundle exists and is not
// near expiry, writing the server Secret (mounted into the AGC) and the scraper
// client Secret (published for monitoring). Mirrors v1's ensureMetricsCerts.
func (r *ActionsGatewayV2Reconciler) ensureMetricsCerts(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) error {
	var serverSec corev1.Secret
	serverErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: metricsTLSSecretNameV2(ag)}, &serverSec)
	if serverErr != nil && !apierrors.IsNotFound(serverErr) {
		return serverErr
	}
	var clientSec corev1.Secret
	clientErr := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: metricsClientSecretNameV2(ag)}, &clientSec)
	if clientErr != nil && !apierrors.IsNotFound(clientErr) {
		return clientErr
	}

	if !apierrors.IsNotFound(serverErr) && !apierrors.IsNotFound(clientErr) {
		if cert, err := parseCertPEM(serverSec.Data[corev1.TLSCertKey]); err == nil {
			if time.Until(cert.NotAfter) > metricsCertRenewBefore {
				return nil
			}
		}
	}

	bundle, err := generateMetricsCertsV2(ag.Namespace, agcNameV2(ag))
	if err != nil {
		return fmt.Errorf("generate metrics certs: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ag, buildMetricsTLSSecretV2(ag, bundle)); err != nil {
		return fmt.Errorf("metrics server Secret: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ag, buildMetricsClientSecretV2(ag, bundle)); err != nil {
		return fmt.Errorf("metrics client Secret: %w", err)
	}
	return nil
}

// --- apply helpers (CreateOrPatch + controller owner reference) ---

func (r *ActionsGatewayV2Reconciler) applyServiceAccount(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *corev1.ServiceAccount) error {
	obj := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	return err
}

func (r *ActionsGatewayV2Reconciler) applyRoleBinding(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *rbacv1.RoleBinding) error {
	obj := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		// roleRef is immutable; on a roleRef change the binding must be recreated.
		if obj.ResourceVersion != "" && obj.RoleRef != desired.RoleRef {
			return errRoleRefImmutable
		}
		obj.Labels = desired.Labels
		obj.RoleRef = desired.RoleRef
		obj.Subjects = desired.Subjects
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	if errors.Is(err, errRoleRefImmutable) {
		if delErr := r.Delete(ctx, obj); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
		_ = controllerutil.SetControllerReference(ag, desired, r.Scheme)
		return r.Create(ctx, desired)
	}
	return err
}

// applyClusterRunnerTemplateReaderBinding creates or patches the per-gateway
// ClusterRoleBinding granting the AGC SA cluster-scoped read of ClusterRunnerTemplate.
// No owner reference: a cluster-scoped object cannot be owned by a namespaced
// ActionsGateway (the apiserver rejects the cross-scope ref and never GCs it), so
// reconcileDelete removes it explicitly.
func (r *ActionsGatewayV2Reconciler) applyClusterRunnerTemplateReaderBinding(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) error {
	desired := buildClusterRunnerTemplateReaderBinding(ag)
	obj := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		// roleRef is immutable; on a roleRef change the binding must be recreated.
		if obj.ResourceVersion != "" && obj.RoleRef != desired.RoleRef {
			return errRoleRefImmutable
		}
		obj.Labels = desired.Labels
		obj.RoleRef = desired.RoleRef
		obj.Subjects = desired.Subjects
		return nil
	})
	if errors.Is(err, errRoleRefImmutable) {
		if delErr := r.Delete(ctx, obj); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
		return r.Create(ctx, desired)
	}
	return err
}

func (r *ActionsGatewayV2Reconciler) applyService(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *corev1.Service) error {
	obj := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec.Type = desired.Spec.Type
		obj.Spec.Selector = desired.Spec.Selector
		obj.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	return err
}

func (r *ActionsGatewayV2Reconciler) applyNetworkPolicy(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *networkingv1.NetworkPolicy) error {
	obj := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	return err
}

func (r *ActionsGatewayV2Reconciler) applyDeployment(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *appsv1.Deployment) error {
	obj := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	return err
}

func (r *ActionsGatewayV2Reconciler) applyOwnedSecret(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, desired *corev1.Secret) error {
	obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.Labels = desired.Labels
		obj.Type = desired.Type
		obj.Data = desired.Data
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	return err
}

// --- status ---

// updateStatus reads the AGC Deployment readiness and writes the uniform v2
// status/condition contract: Ready + AGCAvailable, observedGeneration, and a
// cleared CredentialUnavailable + Degraded (provisioning reached here).
func (r *ActionsGatewayV2Reconciler) updateStatus(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) (ctrl.Result, error) {
	var dep appsv1.Deployment
	agcReady := false
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}, &dep); err == nil {
		agcReady = dep.Status.ReadyReplicas >= 1
	}
	now := metav1.Now()
	gen := ag.Generation
	set := func(condType string, status bool, reason, msg string) {
		s := metav1.ConditionFalse
		if status {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
			Type: condType, Status: s, Reason: reason, Message: msg, LastTransitionTime: now, ObservedGeneration: gen,
		})
	}

	// Provisioning succeeded, so clear the abnormal conditions.
	set(gmcv2alpha1.ConditionCredentialUnavailable, false, gmcv2alpha1.ReasonReconcileSucceeded, "GitHub App Secret present")
	set(gmcv2alpha1.ConditionDegraded, false, gmcv2alpha1.ReasonReconcileSucceeded, "all AGC control-plane resources reconciled")

	agcReason := gmcv2alpha1.ReasonAGCReady
	agcMsg := "AGC Deployment has a ready replica"
	if !agcReady {
		agcReason = gmcv2alpha1.ReasonAGCReady
		agcMsg = "AGC Deployment has no ready replica yet"
	}
	set(gmcv2alpha1.ConditionAGCAvailable, agcReady, agcReason, agcMsg)

	readyReason := gmcv2alpha1.ReasonReady
	readyMsg := "AGC control plane is available"
	if !agcReady {
		readyReason = gmcv2alpha1.ReasonAGCReady
		readyMsg = "waiting for the AGC Deployment to become ready"
	}
	set(gmcv2alpha1.ConditionReady, agcReady, readyReason, readyMsg)

	ag.Status.ObservedGeneration = gen
	if err := r.Status().Update(ctx, ag); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	if !agcReady {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// setNotReady records a fail-closed condition (CredentialUnavailable / ProxyNotFound)
// plus Ready=False with the same reason, before provisioning any children.
func (r *ActionsGatewayV2Reconciler) setNotReady(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, condType, reason, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	gen := ag.Generation
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type: condType, Status: metav1.ConditionTrue, Reason: reason, Message: msg, LastTransitionTime: now, ObservedGeneration: gen,
	})
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg, LastTransitionTime: now, ObservedGeneration: gen,
	})
	ag.Status.ObservedGeneration = gen
	if err := r.Status().Update(ctx, ag); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// setDegraded records Degraded=True naming the failing step and returns the cause
// so the work item is retried with backoff (mirrors the EgressProxy reconciler).
func (r *ActionsGatewayV2Reconciler) setDegraded(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, cause error) (ctrl.Result, error) {
	now := metav1.Now()
	gen := ag.Generation
	for _, t := range []string{gmcv2alpha1.ConditionDegraded, gmcv2alpha1.ConditionReady} {
		status := metav1.ConditionTrue
		if t == gmcv2alpha1.ConditionReady {
			status = metav1.ConditionFalse
		}
		meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
			Type: t, Status: status, Reason: gmcv2alpha1.ReasonProvisioningFailed,
			Message: cause.Error(), LastTransitionTime: now, ObservedGeneration: gen,
		})
	}
	ag.Status.ObservedGeneration = gen
	if err := r.Status().Update(ctx, ag); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, cause
}

// reconcileDelete deletes the cluster-scoped resources the apiserver cannot
// garbage-collect via owner reference, then removes the finalizer. The namespaced
// AGC control-plane children carry a controller owner reference, so the apiserver
// GCs them once the CR is gone — and because every name is per-gateway (§H.16 #1),
// deleting one gateway GCs only its own children, never a neighbor's. The
// ClusterRunnerTemplate ClusterRoleBinding is cluster-scoped and cannot carry an
// owner ref to a namespaced object, so it is deleted explicitly here. RunnerSets
// reference the gateway but are not owned by it, so they are not deleted — they
// degrade to Ready=False/GatewayNotFound via their own watch.
func (r *ActionsGatewayV2Reconciler) reconcileDelete(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) (ctrl.Result, error) {
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterRunnerTemplateReaderBindingName(ag)}}
	if err := r.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete ClusterRunnerTemplate ClusterRoleBinding: %w", err)
	}

	controllerutil.RemoveFinalizer(ag, gmcv2alpha1.ActionsGatewayFinalizer)
	if err := r.Update(ctx, ag); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the v2 ActionsGateway reconciler: it owns the AGC
// Deployment (so its readiness change refreshes status), watches the credential
// Secret (metadata-only — no secret bodies cached), and watches EgressProxy so a
// gateway sitting Ready=False/ProxyNotFound flips when its defaultProxyRef'd proxy
// appears.
func (r *ActionsGatewayV2Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gmcv2alpha1.ActionsGateway{}).
		Owns(&appsv1.Deployment{}).
		WatchesMetadata(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToActionsGateways),
		).
		Watches(
			&gmcv2alpha1.EgressProxy{},
			handler.EnqueueRequestsFromMapFunc(r.proxyToActionsGateways),
		).
		Named("actionsgateway-v2").
		Complete(r)
}

// secretToActionsGateways enqueues any v2 ActionsGateway in the Secret's namespace
// whose githubAppRef names it, so a credential Secret create/delete re-reconciles.
func (r *ActionsGatewayV2Reconciler) secretToActionsGateways(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.gatewaysMatching(ctx, obj.GetNamespace(), func(ag *gmcv2alpha1.ActionsGateway) bool {
		return ag.Spec.GitHubAppRef.Name == obj.GetName()
	})
}

// proxyToActionsGateways enqueues any v2 ActionsGateway whose defaultProxyRef names
// this EgressProxy.
func (r *ActionsGatewayV2Reconciler) proxyToActionsGateways(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.gatewaysMatching(ctx, obj.GetNamespace(), func(ag *gmcv2alpha1.ActionsGateway) bool {
		return ag.Spec.DefaultProxyRef != nil && ag.Spec.DefaultProxyRef.Name == obj.GetName()
	})
}

func (r *ActionsGatewayV2Reconciler) gatewaysMatching(ctx context.Context, ns string, match func(*gmcv2alpha1.ActionsGateway) bool) []ctrl.Request {
	var list gmcv2alpha1.ActionsGatewayList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		ag := &list.Items[i]
		if match(ag) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}})
		}
	}
	return reqs
}
