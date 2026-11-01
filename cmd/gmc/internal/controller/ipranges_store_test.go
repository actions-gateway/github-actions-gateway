package controller

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const storeTestNamespace = "gmc-system"

func newTestIPRangeStore(t *testing.T, objs ...client.Object) (*IPRangeStore, client.Client) {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(newIPRangeScheme(t)).WithObjects(objs...).Build()
	return &IPRangeStore{Reader: fc, Writer: fc, Namespace: storeTestNamespace, Name: IPRangeStoreName, MaxAge: 49 * time.Hour}, fc
}

func storeConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: storeTestNamespace, Name: IPRangeStoreName},
		Data:       data,
	}
}

func TestIPRangeStore_SaveThenLoad(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestIPRangeStore(t)
	fetchedAt := time.Date(2026, 10, 24, 9, 30, 0, 123, time.UTC)
	cidrs := []net.IPNet{parseCIDR(t, "140.82.112.0/20"), parseCIDR(t, "2a0a:a440::/29")}

	require.NoError(t, store.Save(ctx, cidrs, fetchedAt))
	got, gotAt, err := store.Load(ctx, fetchedAt.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, cidrs, got)
	assert.True(t, gotAt.Equal(fetchedAt), "fetch time must round-trip, got %s", gotAt)

	// A second save overwrites the first rather than failing on AlreadyExists.
	newer := []net.IPNet{parseCIDR(t, "192.30.252.0/22")}
	require.NoError(t, store.Save(ctx, newer, fetchedAt.Add(24*time.Hour)))
	got, _, err = store.Load(ctx, fetchedAt.Add(25*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, newer, got)
}

func TestIPRangeStore_LoadMissingSeedsNothing(t *testing.T) {
	store, _ := newTestIPRangeStore(t)
	got, _, err := store.Load(context.Background(), time.Now())
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestIPRangeStore_LoadFailsClosed pins that every reason to distrust a persisted
// copy yields no CIDRs at all, never the parseable subset: a seeded set must not
// be wider than, or older than, what the running GMC would itself still serve.
func TestIPRangeStore_LoadFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	cases := map[string]map[string]string{
		"corrupt entry beside good ones": {"cidrs": "140.82.112.0/20\n0.0.0.0/0x\n192.30.252.0/22", "fetchedAt": stamp(-time.Hour)},
		"bare address, not a CIDR":       {"cidrs": "140.82.112.1", "fetchedAt": stamp(-time.Hour)},
		"empty set":                      {"cidrs": "", "fetchedAt": stamp(-time.Hour)},
		"no fetch time":                  {"cidrs": "140.82.112.0/20"},
		"unparseable fetch time":         {"cidrs": "140.82.112.0/20", "fetchedAt": "yesterday"},
		"fetch time in the future":       {"cidrs": "140.82.112.0/20", "fetchedAt": stamp(time.Minute)},
		"older than the maximum age":     {"cidrs": "140.82.112.0/20", "fetchedAt": stamp(-49*time.Hour - time.Second)},
		"exactly the maximum age":        {"cidrs": "140.82.112.0/20", "fetchedAt": stamp(-49 * time.Hour)},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			store, _ := newTestIPRangeStore(t, storeConfigMap(data))
			got, _, err := store.Load(context.Background(), now)
			require.Error(t, err)
			assert.Empty(t, got)

			cache := &IPRangeCache{}
			n, err := store.SeedCache(context.Background(), cache, now)
			require.Error(t, err)
			assert.Zero(t, n)
			assert.Empty(t, cache.Snapshot())
			_, refreshed := cache.LastRefresh()
			assert.False(t, refreshed)
		})
	}

	t.Run("no maximum age configured", func(t *testing.T) {
		store, _ := newTestIPRangeStore(t, storeConfigMap(map[string]string{"cidrs": "140.82.112.0/20", "fetchedAt": stamp(-time.Minute)}))
		store.MaxAge = 0
		got, _, err := store.Load(context.Background(), now)
		require.Error(t, err)
		assert.Empty(t, got)
	})
}

// A hand-written copy ending in a newline is read, not refused as an empty entry.
func TestIPRangeStore_LoadToleratesTrailingNewline(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	store, _ := newTestIPRangeStore(t, storeConfigMap(map[string]string{
		"cidrs": "140.82.112.0/20\n192.30.252.0/22\n", "fetchedAt": now.Add(-time.Hour).Format(time.RFC3339Nano),
	}))
	got, _, err := store.Load(context.Background(), now)
	require.NoError(t, err)
	assert.Equal(t, []net.IPNet{parseCIDR(t, "140.82.112.0/20"), parseCIDR(t, "192.30.252.0/22")}, got)
}

func TestIPRangeStore_SeedCacheServesUntilExpiry(t *testing.T) {
	fetchedAt := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	store, _ := newTestIPRangeStore(t, storeConfigMap(map[string]string{
		"cidrs": "140.82.112.0/20", "fetchedAt": fetchedAt.Format(time.RFC3339Nano),
	}))
	clock := fetchedAt.Add(48 * time.Hour)
	cache := &IPRangeCache{now: func() time.Time { return clock }}

	n, err := store.SeedCache(context.Background(), cache, clock)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []net.IPNet{parseCIDR(t, "140.82.112.0/20")}, cache.Snapshot())
	last, ok := cache.LastRefresh()
	require.True(t, ok)
	assert.True(t, last.Equal(fetchedAt), "seeded cache must report the persisted fetch time, got %s", last)

	// At fetchedAt+MaxAge the seed is no longer served, though no fetch has landed.
	clock = fetchedAt.Add(store.MaxAge)
	assert.Empty(t, cache.Snapshot())
}

func TestIPRangeCache_FetchReplacesSeed(t *testing.T) {
	fetchedAt := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	clock := fetchedAt
	cache := &IPRangeCache{now: func() time.Time { return clock }}
	require.True(t, cache.Seed([]net.IPNet{parseCIDR(t, "140.82.112.0/20")}, fetchedAt, fetchedAt.Add(time.Hour)))

	fresh := []net.IPNet{parseCIDR(t, "192.30.252.0/22")}
	cache.Set(fresh)
	cache.MarkRefreshed(fetchedAt.Add(time.Minute))

	// A fetched set carries no expiry: it is served past the seed's.
	clock = fetchedAt.Add(2 * time.Hour)
	assert.Equal(t, fresh, cache.Snapshot())
}

func TestIPRangeCache_SeedNeverOverwritesAFetch(t *testing.T) {
	cache := &IPRangeCache{}
	fresh := []net.IPNet{parseCIDR(t, "192.30.252.0/22")}
	cache.Set(fresh)
	cache.MarkRefreshed(time.Now())

	assert.False(t, cache.Seed([]net.IPNet{parseCIDR(t, "140.82.112.0/20")}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	assert.Equal(t, fresh, cache.Snapshot())
}

func TestIPRangeReconciler_PersistsSuccessfulFetch(t *testing.T) {
	ctx := context.Background()
	store, fc := newTestIPRangeStore(t)
	cidrs := []net.IPNet{parseCIDR(t, "140.82.112.0/20")}
	r := &IPRangeReconciler{Client: fc, Fetcher: &stubFetcher{cidrs: cidrs}, Cache: &IPRangeCache{}, Store: store}

	before := time.Now()
	require.NoError(t, r.reconcileAll(ctx, slog.Default()))
	got, fetchedAt, err := store.Load(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, cidrs, got)
	assert.False(t, fetchedAt.Before(before))
}

func TestIPRangeReconciler_FailedFetchLeavesStoreAlone(t *testing.T) {
	ctx := context.Background()
	stamp := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	store, fc := newTestIPRangeStore(t, storeConfigMap(map[string]string{"cidrs": "140.82.112.0/20", "fetchedAt": stamp}))
	r := &IPRangeReconciler{Client: fc, Fetcher: &stubFetcher{err: errors.New("network error")}, Cache: &IPRangeCache{}, Store: store}

	require.Error(t, r.reconcileAll(ctx, slog.Default()))
	var cm corev1.ConfigMap
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: storeTestNamespace, Name: IPRangeStoreName}, &cm))
	assert.Equal(t, stamp, cm.Data["fetchedAt"])
}
