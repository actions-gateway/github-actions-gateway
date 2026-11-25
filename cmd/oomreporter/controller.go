package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/controller"
)

const (
	// The policies are in force a while after they are made, which is after the controller is.
	guardedWithin, guardedEvery = time.Minute, 2 * time.Second
	syncedWithin                = 2 * time.Minute
	againEvery                  = time.Minute
	stoppedIn                   = 10 * time.Second

	// refused is what the report-writer policy says of a ConfigMap that is no report.
	refused = "the oom-reporter writes no ConfigMap but its reports"
)

type controllerOptions struct {
	namespace, agents, namespaces string
	all                           bool
	// For tests, the constants of those names otherwise.
	guardedWithin, guardedEvery, syncedWithin, againEvery time.Duration
}

func controllerFlags(flags *flag.FlagSet, env func(string) string) func(context.Context, *rest.Config, *slog.Logger) error {
	o := controllerOptions{guardedWithin: guardedWithin, guardedEvery: guardedEvery, syncedWithin: syncedWithin, againEvery: againEvery}
	flags.StringVar(&o.namespace, "namespace", env("POD_NAMESPACE"), "the reports' and the agents' namespace (default $POD_NAMESPACE)")
	flags.StringVar(&o.agents, "agents", "", "the labels of the agents' pods, as key=value,key=value")
	flags.StringVar(&o.namespaces, "namespaces", "", "the namespaces whose pods are written to, with commas between")
	flags.BoolVar(&o.all, "all-namespaces", false, "write to the pods of every namespace")
	return func(ctx context.Context, cfg *rest.Config, log *slog.Logger) error { return o.run(ctx, cfg, log) }
}

// run runs the controller on the namespaces it can read, and returns once it can read another.
func (o controllerOptions) run(ctx context.Context, cfg *rest.Config, log *slog.Logger) error {
	r, err := o.reconciler()
	if err != nil {
		return err
	}
	cfg = unlimited(cfg)
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	if err := o.guarded(ctx, kube, log); ctx.Err() != nil || err != nil {
		return err
	}
	without, err := unreadable(ctx, kube, r.Namespaces)
	if err != nil {
		return err
	}
	for name := range without {
		log.Warn("running without a namespace whose pods it may not list", "namespace", name)
		delete(r.Namespaces, name)
	}
	mgr, err := o.manager(ctx, cfg, r)
	if err != nil {
		return err
	}
	log.Info("starting", "namespaces", slices.Sorted(maps.Keys(r.Namespaces)), "allNamespaces", o.all)

	ended := make(chan error, 3)
	go func() { ended <- mgr.Start(ctx) }()
	go func() {
		if err := o.synced(ctx, mgr); err != nil {
			ended <- err
		}
	}()
	go func() {
		if err := o.again(ctx, kube, without); err != nil {
			ended <- err
		}
	}()
	select {
	case err := <-ended:
		return err
	case <-ctx.Done():
	}
	// A manager whose caches have not synced does not stop (v0.25.2). The process ends it.
	select {
	case err := <-ended:
		return err
	case <-time.After(stoppedIn):
		return nil
	}
}

// guarded waits until the admission policies refuse what the grants let through.
func (o controllerOptions) guarded(ctx context.Context, kube kubernetes.Interface, log *slog.Logger) error {
	late := time.After(o.guardedWithin)
	for {
		_, err := kube.CoreV1().ConfigMaps(o.namespace).Create(ctx,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "not-a-report"}}, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), refused) {
			return nil
		}
		log.Info("waiting for the admission policies", "wantRefused", "a ConfigMap that is no report", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-late:
			return errors.New("the admission policies are not in force")
		case <-time.After(o.guardedEvery):
		}
	}
}

// unreadable is those of names whose pods the controller may not list. One tenant's are no reason
// to stop for the others.
func unreadable(ctx context.Context, kube kubernetes.Interface, names map[string]bool) (map[string]bool, error) {
	without := map[string]bool{}
	for name := range names {
		switch _, err := kube.CoreV1().Pods(name).List(ctx, metav1.ListOptions{Limit: 1}); {
		case apierrors.IsForbidden(err):
			without[name] = true
		case err != nil:
			return nil, fmt.Errorf("namespace %s: %w", name, err)
		}
	}
	return without, nil
}

// synced returns an error where the caches have not synced in time, for a grant that went since.
func (o controllerOptions) synced(ctx context.Context, mgr ctrl.Manager) error {
	waiting, cancel := context.WithTimeout(ctx, o.syncedWithin)
	defer cancel()
	if mgr.GetCache().WaitForCacheSync(waiting) || ctx.Err() != nil {
		return nil
	}
	return errors.New("the caches have not synced: is there a namespace whose pods it may not list?")
}

// again returns once one of without can be read. The cache takes no more namespaces, so the
// controller starts over.
func (o controllerOptions) again(ctx context.Context, kube kubernetes.Interface, without map[string]bool) error {
	for len(without) > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.againEvery):
		}
		now, err := unreadable(ctx, kube, without)
		if err == nil && len(now) < len(without) {
			return errors.New("a namespace can be read that could not: starting over")
		}
	}
	return nil
}

func (o controllerOptions) manager(ctx context.Context, cfg *rest.Config, r *controller.Reconciler) (ctrl.Manager, error) {
	mgr, err := ctrl.NewManager(cfg, o.options(r.Namespaces))
	if err != nil {
		return nil, err
	}
	r.Client = mgr.GetClient()
	return mgr, r.SetupWithManager(ctx, mgr)
}

// unlimited is cfg without client-go's own limit, 5 requests a second, which would be for every
// node's pods. The API server's stands.
func unlimited(cfg *rest.Config) *rest.Config {
	cfg = rest.CopyConfig(cfg)
	cfg.QPS = -1
	return cfg
}

func (o controllerOptions) options(names map[string]bool) ctrl.Options {
	return ctrl.Options{
		Cache: o.cache(names),
		// Two of it write the same.
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		// A name is unique for the metrics' sake.
		Controller: config.Controller{SkipNameValidation: new(true)},
	}
}

func (o controllerOptions) reconciler() (*controller.Reconciler, error) {
	if o.namespace == "" {
		return nil, errors.New("want -namespace")
	}
	agents, err := metav1.ParseToLabelSelector(o.agents)
	if err != nil {
		return nil, fmt.Errorf("-agents: %w", err)
	}
	if len(agents.MatchLabels) == 0 || len(agents.MatchExpressions) > 0 {
		return nil, errors.New("want -agents, of key=value alone")
	}
	var names map[string]bool
	for name := range strings.SplitSeq(o.namespaces, ",") {
		if name = strings.TrimSpace(name); name != "" {
			if names == nil {
				names = map[string]bool{}
			}
			names[name] = true
		}
	}
	if o.all == (names != nil) {
		return nil, errors.New("want one of -namespaces and -all-namespaces")
	}
	return &controller.Reconciler{Namespace: o.namespace, Agents: agents.MatchLabels, Namespaces: names, Now: time.Now}, nil
}

// cache holds the reports, the agents, and the pods of names, or every pod where that is nil.
func (o controllerOptions) cache(names map[string]bool) cache.Options {
	pods := cache.ByObject{Transform: slim}
	if names != nil {
		pods.Namespaces = map[string]cache.Config{o.namespace: {}}
		for name := range names {
			pods.Namespaces[name] = cache.Config{}
		}
	}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}:       pods,
		&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{o.namespace: {}}},
	}}
}

// slim drops what the controller does not read of a pod, which is most of it.
func slim(o any) (any, error) {
	pod, ok := o.(*corev1.Pod)
	if !ok {
		return o, nil
	}
	pod.ManagedFields, pod.Annotations, pod.OwnerReferences = nil, nil, nil
	pod.Spec = corev1.PodSpec{NodeName: pod.Spec.NodeName, Resources: pod.Spec.Resources}
	return pod, nil
}
