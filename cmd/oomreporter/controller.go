package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/controller"
)

type controllerOptions struct {
	namespace, agents, namespaces string
	all                           bool
}

func controllerFlags(flags *flag.FlagSet, env func(string) string) func(context.Context, *rest.Config, *slog.Logger) error {
	var o controllerOptions
	flags.StringVar(&o.namespace, "namespace", env("POD_NAMESPACE"), "the reports' and the agents' namespace (default $POD_NAMESPACE)")
	flags.StringVar(&o.agents, "agents", "", "the labels of the agents' pods, as key=value,key=value")
	flags.StringVar(&o.namespaces, "namespaces", "", "the namespaces whose pods are written to, with commas between")
	flags.BoolVar(&o.all, "all-namespaces", false, "write to the pods of every namespace")
	return func(ctx context.Context, cfg *rest.Config, log *slog.Logger) error {
		mgr, err := o.manager(ctx, cfg)
		if err != nil {
			return err
		}
		log.Info("starting", "namespaces", o.namespaces, "allNamespaces", o.all)
		return mgr.Start(ctx)
	}
}

func (o controllerOptions) manager(ctx context.Context, cfg *rest.Config) (ctrl.Manager, error) {
	r, err := o.reconciler()
	if err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	// client-go's own limit is 5 requests a second, which is for every node's pods. The API
	// server's stands.
	cfg.QPS = -1
	mgr, err := ctrl.NewManager(cfg, o.options(r.Namespaces))
	if err != nil {
		return nil, err
	}
	r.Client = mgr.GetClient()
	return mgr, r.SetupWithManager(ctx, mgr)
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
