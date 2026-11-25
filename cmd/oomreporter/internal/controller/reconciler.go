package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const (
	// Component is the name the controller's Events are under.
	Component = "oom-reporter"
	// idleFor is how long a node has no agent before its report is deleted, longer than a
	// rollout of the agents takes to replace one.
	idleFor = 10 * time.Minute
	// eventWithin is how old a kill may be that gets an Event, and skew how far ahead its node's
	// clock.
	eventWithin = 10 * time.Minute
	skew        = time.Minute

	uidIndex = "metadata.uid"
)

// Reconciler reconciles a node: its report, and the pods that names.
type Reconciler struct {
	client.Client
	// Namespace is the controller's own, which holds the reports and the agents.
	Namespace string
	// Agents is the labels of the agents' pods.
	Agents client.MatchingLabels
	// Namespaces is those whose pods get conditions and Events. Nil is all of them.
	Namespaces map[string]bool
	Now        func() time.Time

	idle   map[string]time.Time // by node
	events map[string]time.Time // made, by namespace and name
}

// SetupWithManager registers r, and the index it finds pods by.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, uidIndex, byUID); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).Named("report").
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.ofReport)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.ofPod)).
		Complete(r)
}

func byUID(o client.Object) []string { return []string{string(o.GetUID())} }

func (r *Reconciler) ofReport(_ context.Context, o client.Object) []reconcile.Request {
	node, ok := report.Node(o.GetName())
	if !ok || o.GetNamespace() != r.Namespace {
		return nil
	}
	return request(node)
}

// ofPod is the node of an agent, and of a pod that says a container of it was OOMKilled.
func (r *Reconciler) ofPod(_ context.Context, o client.Object) []reconcile.Request {
	pod, ok := o.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	if killed, _ := containers(pod); len(killed) == 0 && !r.isAgent(pod) {
		return nil
	}
	return request(pod.Spec.NodeName)
}

func request(node string) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: node}}}
}

func (r *Reconciler) isAgent(pod *corev1.Pod) bool {
	if pod.Namespace != r.Namespace {
		return false
	}
	for k, v := range r.Agents {
		if pod.Labels[k] != v {
			return false
		}
	}
	return true
}

// Reconcile keeps the report of the node req names, and says what it says.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	node := req.Name
	agent, err := r.hasAgent(ctx, node)
	if err != nil {
		return reconcile.Result{}, err
	}
	cm := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: report.Name(node)}, cm)
	if apierrors.IsNotFound(err) {
		delete(r.idle, node)
		if !agent {
			return reconcile.Result{}, nil
		}
		cm.Namespace, cm.Name = r.Namespace, report.Name(node)
		// The cache may not have shown the one made a moment ago.
		return reconcile.Result{}, ignore(r.Create(ctx, cm), apierrors.IsAlreadyExists)
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	left, err := r.expire(ctx, node, cm, agent)
	if err != nil {
		return reconcile.Result{}, err
	}
	kills, err := report.Decode(cm.Data[report.Key])
	if err != nil {
		log.FromContext(ctx).Info("skipped a report", "node", node, "reason", err.Error())
		return reconcile.Result{RequeueAfter: left}, nil
	}
	return reconcile.Result{RequeueAfter: left}, r.apply(ctx, node, kills)
}

func (r *Reconciler) hasAgent(ctx context.Context, node string) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Namespace), r.Agents); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == node {
			return true, nil
		}
	}
	return false, nil
}

// expire deletes the report of a node that has had no agent for idleFor, and returns how much of
// that is left.
func (r *Reconciler) expire(ctx context.Context, node string, cm *corev1.ConfigMap, agent bool) (time.Duration, error) {
	if agent {
		delete(r.idle, node)
		return 0, nil
	}
	if r.idle == nil {
		r.idle = map[string]time.Time{}
	}
	since, ok := r.idle[node]
	if !ok {
		since = r.Now()
		r.idle[node] = since
	}
	if left := idleFor - r.Now().Sub(since); left > 0 {
		return left, nil
	}
	delete(r.idle, node)
	return 0, client.IgnoreNotFound(r.Delete(ctx, cm, client.Preconditions{UID: &cm.UID}))
}

// apply says what node's kills say of its pods.
func (r *Reconciler) apply(ctx context.Context, node string, kills []report.Kill) error {
	var uids []string
	by := map[string][]report.Kill{}
	for _, k := range kills {
		if by[k.Pod] == nil {
			uids = append(uids, k.Pod)
		}
		by[k.Pod] = append(by[k.Pod], k)
	}
	var errs []error
	for _, uid := range uids {
		pod, err := r.pod(ctx, uid)
		if err != nil {
			return err
		}
		if pod == nil {
			continue
		}
		if pod.Spec.NodeName != node {
			log.FromContext(ctx).Info("dropped a kill of a pod that is on another node",
				"node", node, "pod", client.ObjectKeyFromObject(pod), "podNode", pod.Spec.NodeName)
			continue
		}
		verdicts, processes := judge(pod, by[uid])
		errs = append(errs, r.writeConditions(ctx, pod, verdicts), r.writeEvents(ctx, pod, verdicts, processes))
	}
	return errors.Join(errs...)
}

// pod returns the pod of that UID, nil where there is none in the namespaces r writes to.
func (r *Reconciler) pod(ctx context.Context, uid string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{uidIndex: uid}); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if p := &pods.Items[i]; r.Namespaces == nil || r.Namespaces[p.Namespace] {
			return p, nil
		}
	}
	return nil, nil
}

func (r *Reconciler) writeConditions(ctx context.Context, pod *corev1.Pod, verdicts []verdict) error {
	var changed []corev1.PodCondition
	for _, v := range verdicts {
		if c, is := v.condition(pod, r.Now()); is {
			changed = append(changed, c)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	// The UID is a precondition: a pod made again under the name is another.
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": pod.UID},
		"status":   map[string]any{"conditions": changed},
	})
	if err != nil {
		return err
	}
	err = r.Status().Patch(ctx, pod.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, body))
	return ignore(err, apierrors.IsNotFound, otherUID)
}

// writeEvents makes at most one Event a minute for a container, and one for no container.
func (r *Reconciler) writeEvents(ctx context.Context, pod *corev1.Pod, verdicts []verdict, processes []time.Time) error {
	for _, v := range verdicts {
		text := fmt.Sprintf("Container %s: %s", v.ended.name, v.text())
		if err := r.writeEvent(ctx, pod, v.ended.field, v.at, eventPrefix+string(v.reason), text); err != nil {
			return err
		}
	}
	for _, at := range processes {
		if err := r.writeEvent(ctx, pod, "", at, reasonProcess, wordsProcess); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) writeEvent(ctx context.Context, pod *corev1.Pod, field string, at time.Time, reason, text string) error {
	now := r.Now()
	for name, made := range r.events {
		if now.Sub(made) > 2*eventWithin {
			delete(r.events, name)
		}
	}
	// The name holds the limit across a restart of the controller, and across two of it.
	name := eventName(pod, field, at)
	if _, made := r.events[pod.Namespace+"/"+name]; made || now.Sub(at) > eventWithin || at.Sub(now) > skew {
		return nil
	}
	err := r.Create(ctx, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: name},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1", Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID, FieldPath: field,
		},
		Type: corev1.EventTypeWarning, Reason: reason, Message: text,
		Source:              corev1.EventSource{Component: Component},
		ReportingController: Component,
		FirstTimestamp:      metav1.NewTime(at), LastTimestamp: metav1.NewTime(at), Count: 1,
	})
	if err = ignore(err, apierrors.IsAlreadyExists, namespaceGoes); err != nil {
		return err
	}
	if r.events == nil {
		r.events = map[string]time.Time{}
	}
	r.events[pod.Namespace+"/"+name] = now
	return nil
}

func eventName(pod *corev1.Pod, field string, at time.Time) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d", pod.UID, field, at.Unix()/60)
	name := pod.Name
	// A name has at most 253 characters.
	if most := 253 - len(".") - 16; len(name) > most {
		name = strings.TrimRight(name[:most], ".-")
	}
	return fmt.Sprintf("%s.%016x", name, h.Sum64())
}

// otherUID reports whether err refuses a patch of a UID that is not the object's (kube-apiserver
// v1.35.0).
func otherUID(err error) bool {
	cause, ok := apierrors.StatusCause(err, metav1.CauseTypeFieldValueInvalid)
	return apierrors.IsInvalid(err) && ok && cause.Field == "metadata.uid"
}

func namespaceGoes(err error) bool {
	return apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause)
}

// ignore is err, or nil where one of is says so.
func ignore(err error, is ...func(error) bool) error {
	for _, f := range is {
		if f(err) {
			return nil
		}
	}
	return err
}
