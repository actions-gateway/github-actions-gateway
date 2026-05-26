// Package controller implements the RunnerGroup reconciler.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	"github.com/karlkfi/github-actions-gateway/agc/internal/agentpool"
	"github.com/karlkfi/github-actions-gateway/agc/internal/listener"
	"github.com/karlkfi/github-actions-gateway/agc/internal/provisioner"
	"github.com/karlkfi/github-actions-gateway/agc/internal/token"
	"github.com/karlkfi/github-actions-gateway/broker"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const finalizerName = "actions-gateway.github.com/agentpool-cleanup"

// conditionUpdate is sent from listener goroutines to the reconciler via a channel.
type conditionUpdate struct {
	namespace string
	name      string
	condition metav1.Condition
}

// channelConditionUpdater implements listener.ConditionUpdater.
type channelConditionUpdater struct {
	ch chan<- conditionUpdate
}

func (u *channelConditionUpdater) SetCondition(namespace, name string, cond metav1.Condition) {
	select {
	case u.ch <- conditionUpdate{namespace: namespace, name: name, condition: cond}:
	default:
		// Drop if channel is full to avoid blocking listener goroutines.
	}
}

// RunnerGroupReconciler reconciles RunnerGroup objects.
type RunnerGroupReconciler struct {
	client.Client
	TokenManager *token.Manager
	Registrar    agentpool.Registrar
	BrokerConfig BrokerConfig
	Metrics      *listener.Metrics
	Log          *slog.Logger
	Provisioner  *provisioner.Provisioner
	AgentKeyType agentpool.KeyType // defaults to KeyTypeEd25519 when empty

	// in-process state; rebuilt from Secrets on restart.
	multiplexersMu sync.Mutex
	multiplexers   map[types.NamespacedName]*listener.Multiplexer
	poolsMu        sync.Mutex
	pools          map[types.NamespacedName]*agentpool.Pool

	conditionCh chan conditionUpdate
}

// BrokerConfig holds the connection parameters for the broker client used by
// listener goroutines.
type BrokerConfig struct {
	BrokerURL     string
	RunnerVersion string
	RunnerOS      string
	RunnerArch    string
	UseV2Flow     bool
	HTTPClient    *http.Client
	// IdleThreshold is the number of consecutive empty polls before a burst
	// listener goroutine shuts down. 0 means the default (50).
	IdleThreshold int
	// RenewJobInterval is the cadence of the per-job RenewJob renewal loop.
	// 0 means the default (60s).
	RenewJobInterval time.Duration
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *RunnerGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.multiplexers == nil {
		r.multiplexers = make(map[types.NamespacedName]*listener.Multiplexer)
	}
	if r.pools == nil {
		r.pools = make(map[types.NamespacedName]*agentpool.Pool)
	}
	if r.conditionCh == nil {
		r.conditionCh = make(chan conditionUpdate, 256)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.RunnerGroup{}).
		Complete(r)
}

// Reconcile is called by controller-runtime on RunnerGroup events.
func (r *RunnerGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.multiplexers == nil {
		r.multiplexers = make(map[types.NamespacedName]*listener.Multiplexer)
	}
	if r.pools == nil {
		r.pools = make(map[types.NamespacedName]*agentpool.Pool)
	}
	if r.conditionCh == nil {
		r.conditionCh = make(chan conditionUpdate, 256)
	}
	log := r.Log.With("namespace", req.Namespace, "name", req.Name)

	// 1. Fetch the RunnerGroup.
	var rg v1alpha1.RunnerGroup
	if err := r.Get(ctx, req.NamespacedName, &rg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Drain pending condition updates from listener goroutines.
	r.drainConditions(ctx, &rg)

	// 3. Handle deletion.
	if !rg.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, &rg)
	}

	// 4. Ensure finalizer.
	if !controllerutil.ContainsFinalizer(&rg, finalizerName) {
		controllerutil.AddFinalizer(&rg, finalizerName)
		if err := r.Update(ctx, &rg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 5. Get installation token for agent management.
	instToken, err := r.TokenManager.Token(ctx)
	if err != nil {
		log.Error("failed to get installation token", "error", err)
		return ctrl.Result{}, err
	}

	// 6. Ensure agent pool Secrets.
	pool := r.getOrCreatePool(req.NamespacedName, rg.Namespace, rg.Name)
	if err := pool.EnsureAgents(ctx, rg.Spec.MaxListeners, instToken); err != nil {
		log.Error("EnsureAgents failed", "error", err)
		return ctrl.Result{}, err
	}

	// 7. Start or update the Multiplexer.
	// Pass a deep copy so the factory closure captures a snapshot that is not
	// subject to concurrent mutation by r.Status().Update below (which zeroes
	// the struct before writing the API response back into it).
	mux := r.getOrCreateMultiplexer(ctx, req.NamespacedName, rg.DeepCopy(), pool)
	mux.SetMaxListeners(rg.Spec.MaxListeners)

	// 8. Update status.
	rg.Status.ActiveSessions = mux.ActiveCount()
	rg.Status.ObservedGeneration = rg.Generation
	r.setReadyCondition(&rg, mux.ActiveCount() > 0)

	if err := r.Status().Update(ctx, &rg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileDelete handles RunnerGroup deletion: stop goroutines, delete Secrets, remove finalizer.
func (r *RunnerGroupReconciler) reconcileDelete(ctx context.Context, log *slog.Logger, rg *v1alpha1.RunnerGroup) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rg.Namespace, Name: rg.Name}

	// Stop the multiplexer if running.
	r.multiplexersMu.Lock()
	if mux, ok := r.multiplexers[key]; ok {
		mux.Stop()
		delete(r.multiplexers, key)
	}
	r.multiplexersMu.Unlock()

	// Delete agent Secrets.
	pool := r.getPool(key)
	if pool != nil {
		instToken, err := r.TokenManager.Token(ctx)
		if err != nil {
			log.Warn("could not get token for pool cleanup; proceeding without deregistration", "error", err)
			instToken = ""
		}
		if err := pool.DeleteAll(ctx, instToken); err != nil {
			return ctrl.Result{}, fmt.Errorf("pool.DeleteAll: %w", err)
		}
		r.poolsMu.Lock()
		delete(r.pools, key)
		r.poolsMu.Unlock()
	}

	controllerutil.RemoveFinalizer(rg, finalizerName)
	if err := r.Update(ctx, rg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// getOrCreatePool returns the Pool for the given RunnerGroup, creating it if needed.
func (r *RunnerGroupReconciler) getOrCreatePool(key types.NamespacedName, namespace, groupName string) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	if p, ok := r.pools[key]; ok {
		return p
	}
	p := agentpool.NewPool(r.Client, namespace, groupName, r.BrokerConfig.RunnerVersion, r.Registrar, r.AgentKeyType)
	r.pools[key] = p
	return p
}

func (r *RunnerGroupReconciler) getPool(key types.NamespacedName) *agentpool.Pool {
	r.poolsMu.Lock()
	defer r.poolsMu.Unlock()
	return r.pools[key]
}

// getOrCreateMultiplexer returns the Multiplexer for the given RunnerGroup, creating and starting it if needed.
func (r *RunnerGroupReconciler) getOrCreateMultiplexer(ctx context.Context, key types.NamespacedName, rg *v1alpha1.RunnerGroup, pool *agentpool.Pool) *listener.Multiplexer {
	r.multiplexersMu.Lock()
	defer r.multiplexersMu.Unlock()

	if mux, ok := r.multiplexers[key]; ok {
		return mux
	}

	condCh := r.conditionCh
	condUpdater := &channelConditionUpdater{ch: condCh}
	brokerCfg := r.BrokerConfig

	factory := func(index int) listener.Config {
		agent := pool.ClaimAgent()
		if agent == nil {
			// Pool exhausted; return minimal config — goroutine will fail quickly.
			return listener.Config{
				Group:     rg.Name,
				Namespace: rg.Namespace,
			}
		}
		bc := &broker.BrokerClient{
			BrokerURL:     brokerCfg.BrokerURL,
			RunnerVersion: brokerCfg.RunnerVersion,
			RunnerOS:      brokerCfg.RunnerOS,
			RunnerArch:    brokerCfg.RunnerArch,
			UseV2Flow:     brokerCfg.UseV2Flow,
			HTTPClient:    brokerCfg.HTTPClient,
		}
		jobHandler := listener.JobHandlerFunc(nil)
		if r.Provisioner != nil {
			jobHandler = r.Provisioner.HandlerFor(rg)
		}
		return listener.Config{
			Group:         rg.Name,
			Namespace:     rg.Namespace,
			Agent:         agent,
			Broker:        bc,
			Conditions:    condUpdater,
			Metrics:       r.Metrics,
			RunnerOS:      brokerCfg.RunnerOS,
			JobHandler:    jobHandler,
			IdleThreshold: brokerCfg.IdleThreshold,
			RenewInterval: brokerCfg.RenewJobInterval,
		}
	}

	mux := listener.NewMultiplexer(factory, rg.Spec.MaxListeners, r.Log)
	if err := mux.Start(ctx); err != nil {
		r.Log.Error("failed to start multiplexer", "error", err)
	}
	r.multiplexers[key] = mux
	return mux
}

// drainConditions reads pending condition updates and merges them into rg.Status.
// Updates for other RunnerGroups are collected and re-enqueued after the loop
// to avoid re-processing them in the current iteration.
func (r *RunnerGroupReconciler) drainConditions(_ context.Context, rg *v1alpha1.RunnerGroup) {
	var skipped []conditionUpdate
	for {
		select {
		case upd := <-r.conditionCh:
			if upd.namespace == rg.Namespace && upd.name == rg.Name {
				r.mergeCondition(rg, upd.condition)
			} else {
				skipped = append(skipped, upd)
			}
		default:
			goto done
		}
	}
done:
	for _, upd := range skipped {
		select {
		case r.conditionCh <- upd:
		default:
			// channel full — condition dropped (best-effort)
		}
	}
}

func (r *RunnerGroupReconciler) mergeCondition(rg *v1alpha1.RunnerGroup, cond metav1.Condition) {
	for i, existing := range rg.Status.Conditions {
		if existing.Type == cond.Type {
			rg.Status.Conditions[i] = cond
			return
		}
	}
	rg.Status.Conditions = append(rg.Status.Conditions, cond)
}

func (r *RunnerGroupReconciler) setReadyCondition(rg *v1alpha1.RunnerGroup, ready bool) {
	status := metav1.ConditionFalse
	reason := "NoActiveSessions"
	msg := "No listener goroutines are running."
	if ready {
		status = metav1.ConditionTrue
		reason = "ListenerActive"
		msg = "At least one listener goroutine is running."
	}
	r.mergeCondition(rg, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rg.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

// SetConditionForTest enqueues a condition update as if it came from a listener
// goroutine. Intended for use in unit tests only.
func (r *RunnerGroupReconciler) SetConditionForTest(ns, name string, cond metav1.Condition) {
	if r.conditionCh == nil {
		return
	}
	select {
	case r.conditionCh <- conditionUpdate{namespace: ns, name: name, condition: cond}:
	default:
	}
}
