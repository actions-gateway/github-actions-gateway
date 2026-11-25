package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/agent"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

type agentOptions struct {
	node, namespace, mount, root string
	poll                         time.Duration
	// mountinfo is /proc/self/mountinfo.
	mountinfo string
}

func agentFlags(flags *flag.FlagSet, env func(string) string) func(context.Context, *rest.Config, *slog.Logger) error {
	o := agentOptions{mountinfo: "/proc/self/mountinfo"}
	flags.StringVar(&o.node, "node", env("NODE_NAME"), "the node's name (default $NODE_NAME)")
	flags.StringVar(&o.namespace, "namespace", env("POD_NAMESPACE"), "the reports' namespace (default $POD_NAMESPACE)")
	flags.StringVar(&o.mount, "cgroup-mount", "/sys/fs/cgroup", "where the node's cgroup v2 hierarchy is mounted")
	flags.StringVar(&o.root, "pods-root", "", "the kubelet's cgroup of all pods below that, where it is not found")
	flags.DurationVar(&o.poll, "poll", 250*time.Millisecond, "the longest time between two samples")
	return func(ctx context.Context, cfg *rest.Config, log *slog.Logger) error {
		if o.node == "" || o.namespace == "" {
			return errors.New("want -node and -namespace")
		}
		if o.poll <= 0 {
			return errors.New("want a -poll above 0")
		}
		kube, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return err
		}
		return o.run(ctx, kube, log)
	}
}

// run reports until ctx is done or the node's cgroups cannot be read.
func (o agentOptions) run(ctx context.Context, kube kubernetes.Interface, log *slog.Logger) error {
	tree, err := o.open()
	if err != nil {
		return err
	}
	writer := agent.NewWriter(o.put(kube), log)
	a := &agent.Agent{Tree: tree, Poll: o.poll, Write: writer.Write, Log: log}
	ctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stop()
	wg.Go(func() { writer.Run(ctx) })
	w, err := agent.NewWatcher(o.mount, tree.PodFile, log)
	if err != nil {
		log.Warn("cannot watch; kills are seen by the poll", "error", err)
	} else {
		a.Watch, a.Changed = w.Follow, w.Changed
		wg.Go(func() { w.Run(ctx) })
	}
	log.Info("sampling", "node", o.node, "poll", o.poll.String())
	return a.Run(ctx)
}

func (o agentOptions) open() (*cgroup.Tree, error) {
	f, err := os.Open(o.mountinfo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	local, err := cgroup.LocalEvents(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.mountinfo, err)
	}
	if local {
		return nil, errors.New("cgroup v2 is mounted memory_localevents, which hides a container's kill from its pod's counters")
	}
	return cgroup.Open(os.DirFS(o.mount), o.root)
}

// put writes the node's report over the one there. It may be updated and not read or made: the
// controller makes it.
func (o agentOptions) put(kube kubernetes.Interface) func(context.Context, string) error {
	return func(ctx context.Context, data string) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := kube.CoreV1().ConfigMaps(o.namespace).Update(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: o.namespace, Name: report.Name(o.node)},
			Data:       map[string]string{report.Key: data},
		}, metav1.UpdateOptions{})
		return err
	}
}
