package controller

import (
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage"
	corev1 "k8s.io/api/core/v1"
)

// RunnerImageResolver answers what runner version a worker image ships, read from
// the image in its registry (Q988). *runnerimage.Resolver is the production
// implementation; tests substitute a fake.
type RunnerImageResolver interface {
	Lookup(runnerimage.Request) runnerimage.Lookup
}

// imageLookup asks the resolver about image, registering wake to be called when the
// answer changes. A nil resolver returns nil, which the verdict reads as "tag alone".
func imageLookup(resolver RunnerImageResolver, image string, pullSecrets []corev1.LocalObjectReference, wake func()) *runnerimage.Lookup {
	if resolver == nil {
		return nil
	}
	names := make([]string, 0, len(pullSecrets))
	for _, s := range pullSecrets {
		if s.Name != "" {
			names = append(names, s.Name)
		}
	}
	lookup := resolver.Lookup(runnerimage.Request{Image: image, PullSecrets: names, Wake: wake})
	return &lookup
}
