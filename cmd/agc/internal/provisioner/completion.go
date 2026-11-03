package provisioner

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deletePod deletes a worker pod, tolerating NotFound (the reaper or an
// external actor may have removed it first).
func (p *Provisioner) deletePod(ctx context.Context, namespace, name string) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
		p.logFor().Warn("failed to delete worker pod", "pod", name, "error", err)
		return err
	}
	return nil
}

func (p *Provisioner) deleteSecret(ctx context.Context, namespace, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
		p.logFor().Warn("failed to delete job Secret", "secret", name, "error", err)
		return err
	}
	return nil
}
