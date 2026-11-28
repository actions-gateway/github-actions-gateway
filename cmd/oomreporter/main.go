// Command oomreporter says which limit ran out when a container is killed for memory.
//
//	oomreporter agent       samples its node's cgroup counters and reports the kills they show
//	oomreporter controller  says on each pod what the reports say of it
//	oomreporter logreader   hands the agent the kernel log's kills, as numbers
//
// The agent reads no pod, the controller no node, and the log reader nothing of the API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	err := run(ctx, os.Args[1:], os.Getenv, rest.InClusterConfig, log)
	stop()
	if err != nil {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("want a mode: agent, controller or logreader")

// run runs the mode args name until ctx is done.
func run(ctx context.Context, args []string, env func(string) string, config func() (*rest.Config, error), log *slog.Logger) error {
	if len(args) == 0 {
		return errUsage
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var start func(context.Context, *rest.Config, *slog.Logger) error
	var offline func(context.Context, *slog.Logger) error
	switch args[0] {
	case "agent":
		start = agentFlags(flags, env)
	case "controller":
		start = controllerFlags(flags, env)
	case "logreader":
		offline = logReaderFlags(flags)
	default:
		return fmt.Errorf("%w, not %q", errUsage, args[0])
	}
	if err := flags.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		flags.SetOutput(os.Stderr)
		flags.Usage()
		return nil
	} else if err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if offline != nil {
		return offline(ctx, log)
	}
	cfg, err := config()
	if err != nil {
		return err
	}
	return start(ctx, cfg, log)
}
