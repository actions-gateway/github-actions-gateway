package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cached(t *testing.T, o controllerOptions) (pods, reports map[string]cache.Config) {
	t.Helper()
	r, err := o.reconciler()
	require.NoError(t, err)
	for obj, by := range o.cache(r.Namespaces).ByObject {
		switch obj.(type) {
		case *corev1.Pod:
			pods = by.Namespaces
			assert.NotNil(t, by.Transform)
		case *corev1.ConfigMap:
			reports = by.Namespaces
		}
	}
	return pods, reports
}

func TestController_ReadsNoMoreNamespacesThanItIsTold(t *testing.T) {
	o := controllerOptions{namespace: "oom", agents: "app=agent,part=b", namespaces: "team-a, team-b"}
	r, err := o.reconciler()
	require.NoError(t, err)
	assert.Equal(t, client.MatchingLabels{"app": "agent", "part": "b"}, r.Agents)
	assert.Equal(t, map[string]bool{"team-a": true, "team-b": true}, r.Namespaces)
	pods, reports := cached(t, o)
	assert.Equal(t, map[string]cache.Config{"oom": {}, "team-a": {}, "team-b": {}}, pods)
	assert.Equal(t, map[string]cache.Config{"oom": {}}, reports)

	o = controllerOptions{namespace: "oom", agents: "app=agent", all: true}
	r, err = o.reconciler()
	require.NoError(t, err)
	assert.Nil(t, r.Namespaces)
	pods, reports = cached(t, o)
	assert.Nil(t, pods)
	assert.Equal(t, map[string]cache.Config{"oom": {}}, reports)
}

func TestController_LeavesTheLimitToTheAPIServer(t *testing.T) {
	cfg, _ := api(t)
	mgr, err := controllerOptions{namespace: "oom", agents: "app=agent", all: true}.manager(t.Context(), cfg)
	require.NoError(t, err)
	assert.Negative(t, mgr.GetConfig().QPS)
	assert.Zero(t, cfg.QPS)
}

func TestController_ServesNothing(t *testing.T) {
	assert.Equal(t, "0", controllerOptions{}.options(nil).Metrics.BindAddress)
	assert.Empty(t, controllerOptions{}.options(nil).HealthProbeBindAddress)
}

func TestSlim_KeepsWhatTheControllerReads(t *testing.T) {
	limits := &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
	got, err := slim(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "u", Labels: map[string]string{"a": "b"}, Annotations: map[string]string{"a": "b"}},
		Spec:       corev1.PodSpec{NodeName: "node-a", Resources: limits, Containers: []corev1.Container{{Name: "runner"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	})
	require.NoError(t, err)
	assert.Equal(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "u", Labels: map[string]string{"a": "b"}},
		Spec:       corev1.PodSpec{NodeName: "node-a", Resources: limits},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}, got)
	got, err = slim("a tombstone")
	require.NoError(t, err)
	assert.Equal(t, "a tombstone", got)
}

// api is an API server with nothing in it, and what is listed of it.
func api(t *testing.T) (*rest.Config, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lists []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answers := map[string]string{
			"/api":  `{"kind":"APIVersions","versions":["v1"]}`,
			"/apis": `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`,
			"/api/v1": `{"kind":"APIResourceList","groupVersion":"v1","resources":[
				{"name":"pods","namespaced":true,"kind":"Pod"},{"name":"configmaps","namespaced":true,"kind":"ConfigMap"}]}`,
		}
		w.Header().Set("Content-Type", "application/json")
		switch answer, ok := answers[r.URL.Path]; {
		case ok:
			_, _ = io.WriteString(w, answer)
		case r.URL.Query().Get("sendInitialEvents") != "":
			// It lists instead.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Invalid","code":422}`)
		case r.URL.Query().Get("watch") != "":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			mu.Lock()
			lists = append(lists, r.Method+" "+r.URL.Path)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[]}`)
		}
	}))
	t.Cleanup(s.Close)
	return &rest.Config{Host: s.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		slices.Sort(lists)
		return slices.Compact(slices.Clone(lists))
	}
}

func TestController_ListsNoMoreThanItIsGranted(t *testing.T) {
	want := []string{
		"GET /api/v1/namespaces/oom/configmaps", "GET /api/v1/namespaces/oom/pods",
		"GET /api/v1/namespaces/team-a/pods", "GET /api/v1/namespaces/team-b/pods",
	}
	cfg, lists := api(t)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"controller", "-namespace=oom", "-agents=app=agent", "-namespaces=team-a,team-b"}, env(nil),
			func() (*rest.Config, error) { return cfg, nil }, noLog)
	}()
	assert.Eventually(t, func() bool { return len(lists()) >= len(want) }, 20*time.Second, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, want, lists())
	stop()
	require.NoError(t, <-done)
}
