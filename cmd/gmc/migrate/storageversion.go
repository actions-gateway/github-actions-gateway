package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/migrate"
)

// storageVersionCommand is the subcommand that runs the 1.10 storage sweep (Q1086).
const storageVersionCommand = "storage-version"

type storageVersionOptions struct {
	apply       bool
	kubeContext string
	assumeYes   bool
}

func parseStorageVersionOptions(args []string, stderr io.Writer) (storageVersionOptions, error) {
	fs := flag.NewFlagSet("gag-migrate "+storageVersionCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts storageVersionOptions
	fs.BoolVar(&opts.apply, "apply", false, "Rewrite every stored object and prune storedVersions. Default is dry-run (report only).")
	fs.StringVar(&opts.kubeContext, "context", "", "kubeconfig context to target. Pins the cluster explicitly; defaults to the current-context (echoed before any --apply write).")
	fs.BoolVar(&opts.assumeYes, "assume-yes", false, "Skip the --apply confirmation prompt (automation). Also settable via ASSUME_YES=1.")
	fs.BoolVar(&opts.assumeYes, "y", false, "Shorthand for --assume-yes.")
	fs.Usage = func() {
		fprintf(stderr, "gag-migrate %s — rewrite every stored actions-gateway.com object at %s and prune\n", storageVersionCommand, migrate.StorageVersion)
		fprintf(stderr, "each CustomResourceDefinition's status.storedVersions to [%s], which v2.0.0's CRDs require.\n", migrate.StorageVersion)
		fprintf(stderr, "Run it after applying the 1.10 CRDs.\n\n")
		fprintf(stderr, "Usage:\n  gag-migrate %s [--context <ctx>] [--apply] [--assume-yes]\n\n", storageVersionCommand)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if os.Getenv("ASSUME_YES") == "1" {
		opts.assumeYes = true
	}
	return opts, nil
}

func runStorageVersion(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	opts, err := parseStorageVersionOptions(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := ctrlconfig.GetConfigWithContext(opts.kubeContext)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{}) // the sweep reads and writes unstructured
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}
	if opts.apply {
		ok, err := confirm("the storage-version sweep", resolveContextName(opts.kubeContext),
			"every actions-gateway.com object, cluster-wide",
			fmt.Sprintf("This writes every object back unchanged, so the apiserver stores it at %s, then sets each CRD's status.storedVersions to [%s]. Nothing is deleted and every version stays served.",
				migrate.StorageVersion, migrate.StorageVersion),
			opts.assumeYes, stdin, stderr)
		if err != nil {
			return err
		}
		if !ok {
			return errAborted
		}
	}
	return sweepStorageVersion(context.Background(), c, opts.apply, stdout, stderr)
}

// sweepStorageVersion runs the sweep and reports it. Split from runStorageVersion so
// it runs against a test client.
func sweepStorageVersion(ctx context.Context, c client.Client, apply bool, stdout, stderr io.Writer) error {
	_, err := migrate.SweepStorageVersion(ctx, c, migrate.SweepOptions{
		Apply: apply,
		Out:   stdout,
		Retry: func(what string, op func() error) error {
			return retryOnTransientWebhookError(ctx, what, stderr, op)
		},
	})
	if err != nil {
		return err
	}
	if apply {
		fprintf(stderr, "\nStorage sweep complete: every actions-gateway.com CRD stores only %s. Guide: docs/operations/upgrade.md\n", migrate.StorageVersion)
		return nil
	}
	fprintf(stderr, "\nDry-run complete — nothing was written. Re-run with --apply to rewrite and prune.\n")
	return nil
}
