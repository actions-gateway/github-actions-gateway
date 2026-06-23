// Command gag-migrate is the one-shot v1alpha1 → v2alpha1 fan-out migration tool
// (docs/operations/migration-v1-to-v2.md, design §H.11). It reads a v1 tenant's
// ActionsGateway + RunnerGroups and emits the decomposed v2 object set
// (ActionsGateway + EgressProxy + RunnerTemplates + RunnerSets) plus the namespace
// metadata patch that relocates the securityProfile and aligns the Q147/domain
// labels.
//
// Dry-run by default — it prints the v2 manifests for review and applies nothing.
// `--apply` creates the v2 objects and patches the namespace. It never reads,
// prints, or copies Secret contents: only the githubAppRef *name* is carried across.
// It never deletes v1 objects — v1 keeps running beside v2 (coexistence / rollback)
// until the operator tears it down per the migration runbook.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/migrate"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// fprintf/fprintln write to a CLI stream, discarding the write error: a failed
// write to stdout/stderr is not actionable and must not mask the operation's own
// result (errcheck would otherwise flag the unchecked fmt.Fprint* returns).
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func fprintln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }

type options struct {
	namespace     string
	allNamespaces bool
	apply         bool
	outputDir     string
}

// newScheme builds the client scheme with the v1 (gmc + agc) and v2 (neutral) kinds
// the migration reads and writes. Shared by the CLI and its tests.
func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gmcv1alpha1.AddToScheme(scheme))
	utilruntime.Must(agcv1alpha1.AddToScheme(scheme))
	utilruntime.Must(v2alpha1.AddToScheme(scheme))
	return scheme
}

// parseOptions parses the CLI flags into options, returning an error for a missing
// target selector. Split from run so the flag surface is unit-testable without a
// cluster connection.
func parseOptions(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("gag-migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts options
	fs.StringVar(&opts.namespace, "namespace", "", "Tenant namespace to migrate (required unless --all-namespaces).")
	fs.StringVar(&opts.namespace, "n", "", "Shorthand for --namespace.")
	fs.BoolVar(&opts.allNamespaces, "all-namespaces", false, "Migrate every namespace that holds a v1 ActionsGateway.")
	fs.BoolVar(&opts.apply, "apply", false, "Apply the v2 object set and patch the namespace. Default is dry-run (print only).")
	fs.StringVar(&opts.outputDir, "output-dir", "", "Dry-run: write per-namespace manifests here instead of stdout.")
	fs.Usage = func() {
		fprintf(stderr, "gag-migrate — fan a v1alpha1 tenant out to the v2alpha1 object set.\n\n")
		fprintf(stderr, "Usage:\n  gag-migrate --namespace <ns> [--apply] [--output-dir <dir>]\n  gag-migrate --all-namespaces [--apply]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.namespace == "" && !opts.allNamespaces {
		fs.Usage()
		return opts, fmt.Errorf("one of --namespace or --all-namespaces is required")
	}
	return opts, nil
}

func run(args []string, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	return migrateAll(context.Background(), c, opts, stdout, stderr)
}

// migrateAll resolves the target namespaces and migrates each. Split from run so the
// whole fan-out flow is unit-testable against a fake client (run only adds the live
// kubeconfig/client plumbing on top).
func migrateAll(ctx context.Context, c client.Client, opts options, stdout, stderr io.Writer) error {
	namespaces, err := targetNamespaces(ctx, c, opts)
	if err != nil {
		return err
	}
	if len(namespaces) == 0 {
		fprintln(stderr, "no namespaces with a v1 ActionsGateway found; nothing to migrate")
		return nil
	}
	for _, ns := range namespaces {
		if err := migrateNamespace(ctx, c, ns, opts, stdout, stderr); err != nil {
			return fmt.Errorf("namespace %q: %w", ns, err)
		}
	}
	return nil
}

// targetNamespaces resolves the set of namespaces to migrate. A single --namespace
// is returned as-is; --all-namespaces discovers every namespace holding at least one
// v1 ActionsGateway (sorted for deterministic output).
func targetNamespaces(ctx context.Context, c client.Client, opts options) ([]string, error) {
	if !opts.allNamespaces {
		return []string{opts.namespace}, nil
	}
	var list gmcv1alpha1.ActionsGatewayList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list ActionsGateways: %w", err)
	}
	seen := map[string]struct{}{}
	for i := range list.Items {
		seen[list.Items[i].Namespace] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out, nil
}

// migrateNamespace reads the v1 object set for one namespace, fans it out, and
// either prints the result (dry-run) or applies it (--apply). When a namespace holds
// more than one v1 gateway (an unsupported v1 state under the singleton webhook, but
// handled safely), each gateway is fanned out independently and the relocated
// securityProfile is the most-restrictive across them (never weakening any tenant's
// posture).
func migrateNamespace(ctx context.Context, c client.Client, ns string, opts options, stdout, stderr io.Writer) error {
	var gateways gmcv1alpha1.ActionsGatewayList
	if err := c.List(ctx, &gateways, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list ActionsGateways: %w", err)
	}
	if len(gateways.Items) == 0 {
		fprintf(stderr, "namespace %q has no v1 ActionsGateway; skipping\n", ns)
		return nil
	}

	var rgList agcv1alpha1.RunnerGroupList
	if err := c.List(ctx, &rgList, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list RunnerGroups: %w", err)
	}
	var nsObj corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: ns}, &nsObj); err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}

	// Compute the most-restrictive profile up front so each per-gateway patch agrees.
	profiles := make([]string, 0, len(gateways.Items))
	for i := range gateways.Items {
		profiles = append(profiles, gateways.Items[i].Spec.SecurityProfile)
	}
	mostRestrictive := migrate.MostRestrictiveProfile(profiles...)

	for i := range gateways.Items {
		gw := &gateways.Items[i]
		groups := groupsForGateway(gw, &gateways, rgList.Items)
		res, err := migrate.FanOut(migrate.Input{
			Namespace:            ns,
			NamespaceLabels:      nsObj.Labels,
			NamespaceAnnotations: nsObj.Annotations,
			Gateway:              gw,
			RunnerGroups:         groups,
		})
		if err != nil {
			return err
		}
		// Override the relocated profile with the namespace-wide most-restrictive
		// value so co-located gateways converge on one posture.
		if res.NamespacePatch != nil {
			res.NamespacePatch.Labels[v2alpha1.SecurityProfileLabel] = mostRestrictive
		}

		for _, w := range res.Warnings {
			fprintf(stderr, "warning [%s/%s]: %s\n", ns, gw.Name, w)
		}

		if opts.apply {
			if err := applyResult(ctx, c, res, stderr); err != nil {
				return err
			}
			fprintf(stderr, "applied v2 object set for gateway %q in namespace %q\n", gw.Name, ns)
			continue
		}
		if err := emitDryRun(res, ns, gw.Name, opts.outputDir, stdout); err != nil {
			return err
		}
	}
	return nil
}

// groupsForGateway returns the RunnerGroups a gateway owns. With a single gateway in
// the namespace (the v1 norm) every RunnerGroup belongs to it. With more than one,
// groups carrying the GMC owner label for this gateway are assigned to it, and any
// unowned group is assigned to the lexically-first gateway so it is migrated exactly
// once rather than duplicated under every gateway.
func groupsForGateway(gw *gmcv1alpha1.ActionsGateway, all *gmcv1alpha1.ActionsGatewayList, groups []agcv1alpha1.RunnerGroup) []agcv1alpha1.RunnerGroup {
	if len(all.Items) == 1 {
		return groups
	}
	first := ""
	for i := range all.Items {
		if first == "" || all.Items[i].Name < first {
			first = all.Items[i].Name
		}
	}
	var out []agcv1alpha1.RunnerGroup
	for i := range groups {
		owner := groups[i].Labels["actions-gateway/owner-name"]
		switch {
		case owner == gw.Name:
			out = append(out, groups[i])
		case owner == "" && gw.Name == first:
			out = append(out, groups[i])
		}
	}
	return out
}

// emitDryRun renders the result to YAML, writing to a per-namespace file under
// outputDir when set, or to stdout otherwise.
func emitDryRun(res *migrate.Result, ns, gwName, outputDir string, stdout io.Writer) error {
	manifest, err := migrate.RenderManifests(res)
	if err != nil {
		return err
	}
	if outputDir == "" {
		fprintln(stdout, manifest)
		return nil
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	path := filepath.Join(outputDir, fmt.Sprintf("%s-%s.yaml", ns, gwName))
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fprintf(stdout, "wrote %s\n", path)
	return nil
}

// applyResult creates the emitted v2 objects and patches the namespace metadata. It
// is additive and idempotent: an object that already exists is left unchanged (a
// re-run never clobbers operator edits), and the namespace patch only adds the v2
// keys (the v1 keys stay for coexistence). Children are applied before referrers for
// readability; reference integrity is a runtime condition, so order is not required.
func applyResult(ctx context.Context, c client.Client, res *migrate.Result, stderr io.Writer) error {
	objs := []client.Object{res.Proxy}
	for _, t := range res.Templates {
		objs = append(objs, t)
	}
	objs = append(objs, res.Gateway)
	for _, s := range res.Sets {
		objs = append(objs, s)
	}
	for _, obj := range objs {
		if obj == nil {
			continue
		}
		if err := c.Create(ctx, obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				fprintf(stderr, "exists, skipped: %s/%s\n", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
				continue
			}
			return fmt.Errorf("create %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return patchNamespace(ctx, c, res.NamespacePatch)
}

// patchNamespace applies the additive label/annotation patch to the tenant
// namespace via a read-modify-write. Only keys present in the patch are set; no key
// is removed, so the v1 markers keep working during coexistence (the VAPs dual-read).
func patchNamespace(ctx context.Context, c client.Client, patch *migrate.NamespacePatch) error {
	if patch == nil || (len(patch.Labels) == 0 && len(patch.Annotations) == 0) {
		return nil
	}
	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: patch.Name}, &ns); err != nil {
		return fmt.Errorf("get namespace %q for patch: %w", patch.Name, err)
	}
	base := ns.DeepCopy()
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	for k, v := range patch.Labels {
		ns.Labels[k] = v
	}
	if len(patch.Annotations) > 0 && ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	for k, v := range patch.Annotations {
		ns.Annotations[k] = v
	}
	if err := c.Patch(ctx, &ns, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch namespace %q: %w", patch.Name, err)
	}
	return nil
}
