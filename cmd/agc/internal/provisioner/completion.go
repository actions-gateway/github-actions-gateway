package provisioner

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (p *Provisioner) deleteSecret(ctx context.Context, namespace, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
		p.logFor().Warn("failed to delete job Secret", "secret", name, "error", err)
		return err
	}
	return nil
}
