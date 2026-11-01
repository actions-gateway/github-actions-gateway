package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update

// IPRangeStoreName is the ConfigMap, in the GMC's own namespace, holding the last
// successfully fetched GitHub IP-range set (Q1227).
const IPRangeStoreName = "actions-gateway-gmc-ip-ranges"

// ConfigMap data keys of the persisted set: newline-separated CIDRs, and the
// RFC 3339 time of the fetch that returned them.
const (
	ipRangeStoreCIDRsKey     = "cidrs"
	ipRangeStoreFetchedAtKey = "fetchedAt"
)

// IPRangeStore persists the last successful GitHub IP-range fetch to a ConfigMap
// the GMC owns, so a restart can seed IPRangeCache before its first
// api.github.com/meta fetch instead of leaving every CIDR-mode pool with no GitHub
// egress until that fetch lands (Q1227).
//
// Loading fails closed: a copy that is unreadable in any entry, empty, stamped in
// the future, or older than MaxAge seeds nothing, so the cache starts empty exactly
// as it would with no store. A seeded set is one a past fetch returned, and the
// cache stops serving it at fetchedAt+MaxAge unless a fetch replaces it first.
type IPRangeStore struct {
	// Reader reads the ConfigMap uncached: the manager cache watches no ConfigMap
	// but the egress destination allowlist, so this is the manager's API reader.
	Reader client.Reader
	// Writer creates and updates the ConfigMap.
	Writer    client.Writer
	Namespace string
	Name      string
	// MaxAge is the oldest a persisted set may be and still seed the cache, and how
	// long a seeded set is served. It matches the EgressRulesStale threshold, so a
	// seeded set never outlives the age at which a running GMC reports its own
	// ranges stale. Zero seeds nothing.
	MaxAge time.Duration
}

// Save writes cidrs and the time of the fetch that returned them, creating the
// ConfigMap on first use.
func (s *IPRangeStore) Save(ctx context.Context, cidrs []net.IPNet, fetchedAt time.Time) error {
	lines := make([]string, len(cidrs))
	for i := range cidrs {
		lines[i] = cidrs[i].String()
	}
	data := map[string]string{
		ipRangeStoreCIDRsKey:     strings.Join(lines, "\n"),
		ipRangeStoreFetchedAtKey: fetchedAt.UTC().Format(time.RFC3339Nano),
	}

	var cm corev1.ConfigMap
	err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: s.Namespace,
				Name:      s.Name,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "actions-gateway-gmc"},
			},
			Data: data,
		}
		return s.Writer.Create(ctx, &cm)
	}
	if err != nil {
		return err
	}
	cm.Data = data
	return s.Writer.Update(ctx, &cm)
}

// Load returns the persisted set and its fetch time. No ConfigMap yields an empty
// set and no error; any reason to distrust the copy yields an error and no CIDRs.
func (s *IPRangeStore) Load(ctx context.Context, now time.Time) ([]net.IPNet, time.Time, error) {
	if s.MaxAge <= 0 {
		return nil, time.Time{}, errors.New("no maximum age configured")
	}
	var cm corev1.ConfigMap
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &cm); err != nil {
		return nil, time.Time{}, client.IgnoreNotFound(err)
	}

	fetchedAt, err := time.Parse(time.RFC3339Nano, cm.Data[ipRangeStoreFetchedAtKey])
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read %s: %w", ipRangeStoreFetchedAtKey, err)
	}
	if fetchedAt.After(now) {
		return nil, time.Time{}, fmt.Errorf("persisted ranges are stamped %s, in the future", fetchedAt.Format(time.RFC3339))
	}
	if age := now.Sub(fetchedAt); age > s.MaxAge {
		return nil, time.Time{}, fmt.Errorf("persisted ranges are %s old, past the %s limit", age.Round(time.Minute), s.MaxAge)
	}

	var cidrs []net.IPNet
	for line := range strings.SplitSeq(cm.Data[ipRangeStoreCIDRsKey], "\n") {
		_, cidr, err := net.ParseCIDR(line)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("read %s: %w", ipRangeStoreCIDRsKey, err)
		}
		cidrs = append(cidrs, *cidr)
	}
	return cidrs, fetchedAt, nil
}

// SeedCache loads the persisted set into cache, returning how many CIDRs it seeded.
// It seeds nothing on error, and nothing into a cache a fetch has already filled.
func (s *IPRangeStore) SeedCache(ctx context.Context, cache *IPRangeCache, now time.Time) (int, error) {
	cidrs, fetchedAt, err := s.Load(ctx, now)
	if err != nil || len(cidrs) == 0 {
		return 0, err
	}
	if !cache.Seed(cidrs, fetchedAt, fetchedAt.Add(s.MaxAge)) {
		return 0, nil
	}
	return len(cidrs), nil
}
