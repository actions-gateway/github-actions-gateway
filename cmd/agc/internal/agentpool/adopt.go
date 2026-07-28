package agentpool

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AdoptLegacyRunnerSetSecrets moves a RunnerSet's agent Secrets from the pre-Q466
// derivation ("agentpool-<name>-<index>", selected by the v1 runner-group label) onto
// SchemeRunnerSet, and stamps owner on each one. It returns the number of Secrets
// moved.
//
// It exists so the Q466 rename does not orphan a live deployment. A v2 install that
// predates the rename already has agent Secrets under the old names, each holding a
// GitHub runner registration this AGC is the only record of. Leaving them behind would
// leak both — a Secret full of key material nothing reaps, and a runner record that
// stays registered and offline forever — and would force every listener to re-register
// on upgrade. Moving the Secret preserves the registration: the agent's `agentId` rides
// along, so scale-down and DeleteAll can still deregister it, and the runner is renamed
// to the SchemeRunnerSet form only when it is next recycled, which deregisters the old
// record first.
//
// Ordering is create-then-delete, so an interruption leaves a duplicate rather than a
// hole: the next call sees the new-scheme Secret, returns early, and the stale legacy
// copy is reaped by its owner's cleanup. The reverse order could lose an agent's only
// copy of its private key.
//
// claimedByRunnerGroup is the safety gate and is required. Before Q466 the v1 and v2
// pools wrote byte-identical names and labels, so nothing about a legacy Secret says
// which API created it — the caller must answer "is a live v1 RunnerGroup of this name
// using these?" from the API types it owns. Adopting a RunnerGroup's Secrets would
// re-create the very collision this fixes, pointed the other way, so it is consulted
// before anything is touched and any error fails closed (nothing is adopted).
func AdoptLegacyRunnerSetSecrets(
	ctx context.Context,
	c client.Client,
	namespace, name string,
	owner []metav1.OwnerReference,
	claimedByRunnerGroup func(context.Context) (bool, error),
) (int, error) {
	// Already migrated (or a set that never had legacy Secrets): the RunnerSet-scheme
	// Secrets are authoritative, so never look at the legacy names again. This is also
	// the steady-state fast path — one metadata list per reconcile, no bodies.
	current, err := listAgentSecretMeta(ctx, c, namespace, runnerSetOwnerLabels(name))
	if err != nil {
		return 0, fmt.Errorf("agentpool: list runner-set agent Secrets: %w", err)
	}
	if len(current) > 0 {
		return 0, nil
	}

	legacy, err := listAgentSecretMeta(ctx, c, namespace, map[string]string{
		labelManagedBy:   managedByValue,
		labelRunnerGroup: name,
	})
	if err != nil {
		return 0, fmt.Errorf("agentpool: list legacy agent Secrets: %w", err)
	}
	if len(legacy) == 0 {
		return 0, nil
	}

	claimed, err := claimedByRunnerGroup(ctx)
	if err != nil {
		return 0, fmt.Errorf("agentpool: check whether a v1 RunnerGroup owns the legacy agent Secrets: %w", err)
	}
	if claimed {
		slog.Info("agentpool: leaving legacy agent Secrets to the v1 RunnerGroup of the same name; "+
			"this RunnerSet provisions its own under the runner-set naming scheme",
			"namespace", namespace, "name", name, "secrets", len(legacy))
		return 0, nil
	}

	adopted := 0
	for _, m := range legacy {
		idx, err := strconv.Atoi(m.Labels[labelAgentIndex])
		if err != nil {
			// No index label means the pool could never load it as an agent anyway;
			// leave it for an operator rather than guessing a name for it.
			slog.Warn("agentpool: skipping legacy agent Secret with no readable agent index",
				"namespace", namespace, "secret", m.Name)
			continue
		}
		moved, err := adoptOneLegacySecret(ctx, c, namespace, name, m.Name, idx, owner)
		if err != nil {
			return adopted, err
		}
		if moved {
			adopted++
		}
	}
	if adopted > 0 {
		slog.Info("agentpool: adopted legacy agent Secrets onto the runner-set naming scheme",
			"namespace", namespace, "name", name, "count", adopted)
	}
	return adopted, nil
}

// adoptOneLegacySecret copies one legacy agent Secret to its SchemeRunnerSet name and
// deletes the original. It reports whether a copy was written.
func adoptOneLegacySecret(
	ctx context.Context,
	c client.Client,
	namespace, setName, legacyName string,
	index int,
	owner []metav1.OwnerReference,
) (bool, error) {
	var src corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: legacyName}, &src); err != nil {
		if errors.IsNotFound(err) {
			return false, nil // raced with a delete; nothing to carry over
		}
		return false, fmt.Errorf("agentpool: read legacy agent Secret %s: %w", legacyName, err)
	}

	labels := runnerSetOwnerLabels(setName)
	labels[labelAgentIndex] = strconv.Itoa(index)
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            runnerSetSecretName(setName, index),
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: owner,
		},
		Data: src.Data,
	}
	created := true
	if err := c.Create(ctx, dst); err != nil {
		if !errors.IsAlreadyExists(err) {
			return false, fmt.Errorf("agentpool: write adopted agent Secret %s: %w", dst.Name, err)
		}
		// A previous interrupted run already wrote it; fall through to the delete so
		// the duplicate does not linger.
		created = false
	}
	if err := c.Delete(ctx, &src); err != nil && !errors.IsNotFound(err) {
		return created, fmt.Errorf("agentpool: delete legacy agent Secret %s after adoption: %w", legacyName, err)
	}
	return created, nil
}

// listAgentSecretMeta enumerates agent Secrets matching selector as metadata only,
// for the same reason Pool.listSecretMeta does: no agent private key or JIT config is
// pulled over the wire by a bulk list.
func listAgentSecretMeta(ctx context.Context, c client.Client, namespace string, selector map[string]string) ([]metav1.PartialObjectMetadata, error) {
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels(selector)); err != nil {
		return nil, err
	}
	return list.Items, nil
}
