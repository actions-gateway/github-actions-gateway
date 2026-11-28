package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/logreader"
)

type logReaderOptions struct {
	socket, root string
	every        time.Duration
}

func logReaderFlags(flags *flag.FlagSet) func(context.Context, *slog.Logger) error {
	var o logReaderOptions
	flags.StringVar(&o.socket, "socket", "", "the agent's socket, which it is handed each line's numbers on")
	flags.StringVar(&o.root, "pods-root", "", "the kubelet's cgroup of all pods, as the agent is given it")
	flags.DurationVar(&o.every, "every", 2*time.Second, "how often the kernel's log is read")
	return func(ctx context.Context, log *slog.Logger) error {
		if o.socket == "" || o.root == "" {
			return errors.New("want -socket and -pods-root")
		}
		if o.every <= 0 {
			return errors.New("want an -every above 0")
		}
		return o.run(ctx, logreader.ReadAll, logreader.Now, log)
	}
}

// run reads the log every o.every until ctx is done. A log refused at the start is one this
// container may not read: CAP_SYSLOG is missing, or a security module refuses it.
func (o logReaderOptions) run(ctx context.Context, read func() (string, error), now func() (time.Time, time.Duration), log *slog.Logger) error {
	if _, err := read(); err != nil {
		return err
	}
	sender := &logreader.Sender{Path: o.socket}
	r := &logreader.Reader{Root: o.root, Log: read, Now: now, Send: sender.Send, Logs: log}
	log.Info("reading the kernel's log", "every", o.every.String())
	tick := time.NewTicker(o.every)
	defer tick.Stop()
	for {
		if err := r.Once(); err != nil {
			log.Warn("cannot hand the agent the log's kills", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
