package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
	cfg, _ := api(t, nil)
	o := controllerOptions{namespace: "oom", agents: "app=agent", all: true}
	r, err := o.reconciler()
	require.NoError(t, err)
	mgr, err := o.manager(t.Context(), cfg, r)
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

// api is an API server with nothing in it and the policies in force, which answers nothing that
// forbids says it does, and what is listed of it.
func api(t *testing.T, forbids func(*http.Request) bool) (*rest.Config, func() []string) {
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
		forbidden := func(message string) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"`+message+`"}`)
		}
		switch answer, ok := answers[r.URL.Path]; {
		case forbids != nil && forbids(r):
			forbidden("by RBAC")
		case r.Method == http.MethodPost && r.URL.Query().Get("dryRun") == metav1.DryRunAll:
			forbidden("ValidatingAdmissionPolicy 'x' denied request: " + refused)
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
	t.Cleanup(func() {
		// Close waits for the watches of what does not end with the test.
		_ = s.Listener.Close()
		s.CloseClientConnections()
		s.Close()
	})
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
	cfg, lists := api(t, nil)
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
	require.NoError(t, first(t, done))
}

// soon are the options of a controller of namespaces that looks again soon.
func soon(namespaces string) controllerOptions {
	return controllerOptions{namespace: "oom", agents: "app=agent", namespaces: namespaces, waits: waits{
		guardedWithin: time.Minute, guardedEvery: 10 * time.Millisecond, syncedWithin: time.Minute, againEvery: 20 * time.Millisecond, stoppedIn: time.Minute,
	}}
}

// None is a loop without a wait, or one that ends at once.
func TestController_Waits(t *testing.T) {
	v := reflect.ValueOf(usual)
	for i := range v.NumField() {
		assert.GreaterOrEqual(t, time.Duration(v.Field(i).Int()), time.Second, v.Type().Field(i).Name)
	}
	assert.Greater(t, usual.guardedWithin, usual.guardedEvery)
}

// ended is what run returns, which is to be soon.
func ended(t *testing.T, run func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	return first(t, done)
}

func first(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("it runs still")
		return nil
	}
}

func TestController_DoesNothingWithoutThePolicies(t *testing.T) {
	cfg, lists := api(t, func(r *http.Request) bool { return r.Method == http.MethodPost })
	o := soon("team-a")
	o.guardedWithin = 100 * time.Millisecond
	require.Error(t, ended(t, func() error { return o.run(t.Context(), cfg, noLog) }))
	assert.Empty(t, lists())
}

// An operator is told to uninstall a controller whose policies are not in force.
func TestController_SaysThatThePoliciesAreNotInForceOnlyOfAWriteLetThrough(t *testing.T) {
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			if code == http.StatusCreated {
				_, _ = io.WriteString(w, `{"kind":"ConfigMap","apiVersion":"v1"}`)
			}
		}
	}
	for name, tc := range map[string]struct {
		answer  http.HandlerFunc
		unknown bool
	}{
		"let through":       {status(http.StatusCreated), false},
		"there already":     {status(http.StatusConflict), false},
		"refused by RBAC":   {status(http.StatusForbidden), true},
		"unauthenticated":   {status(http.StatusUnauthorized), true},
		"failed":            {status(http.StatusInternalServerError), true},
		"never answered":    {func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, true},
		"not answered late": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			var asked atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The server sees a client go only once it has read what it sent.
				_, _ = io.Copy(io.Discard, r.Body)
				switch {
				case tc.answer != nil:
					tc.answer(w, r)
				case asked.Add(1) == 1:
					status(http.StatusCreated)(w, r)
				default:
					<-r.Context().Done()
				}
			}))
			t.Cleanup(s.Close)
			o := soon("team-a")
			o.guardedWithin = 300 * time.Millisecond
			err := ended(t, func() error { return o.run(t.Context(), &rest.Config{Host: s.URL}, noLog) })
			if tc.unknown {
				require.ErrorContains(t, err, "cannot tell whether")
				return
			}
			require.ErrorContains(t, err, "policies are not in force")
			assert.NotContains(t, err.Error(), "cannot tell")
		})
	}
}

func TestController_WaitsForThePolicies(t *testing.T) {
	var tries atomic.Int32
	cfg, lists := api(t, func(r *http.Request) bool { return r.Method == http.MethodPost && tries.Add(1) < 4 })
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- soon("team-a").run(ctx, cfg, noLog) }()
	assert.Eventually(t, func() bool { return len(lists()) >= 3 }, 20*time.Second, 20*time.Millisecond)
	assert.Equal(t, int32(4), tries.Load())
	stop()
	require.NoError(t, first(t, done))
}

func TestController_StopsWhileItWaitsForThePolicies(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	cfg, _ := api(t, func(r *http.Request) bool { stop(); return r.Method == http.MethodPost })
	o := soon("team-a")
	o.guardedWithin, o.guardedEvery = time.Hour, time.Hour
	require.NoError(t, ended(t, func() error { return o.run(ctx, cfg, noLog) }))
}

// Who may edit a namespace can take its RoleBinding away.
func TestController_RunsWithoutANamespaceItMayNotList(t *testing.T) {
	var bound, read, cached atomic.Bool
	cfg, lists := api(t, func(r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/namespaces/team-b/") {
			return false
		}
		if r.URL.Query().Get("limit") != "1" {
			read.Store(true)
			cached.Store(bound.Load())
		}
		return !bound.Load()
	})
	o := soon("team-a,team-b")
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- o.run(ctx, cfg, noLog) }()
	want := []string{"GET /api/v1/namespaces/oom/configmaps", "GET /api/v1/namespaces/oom/pods", "GET /api/v1/namespaces/team-a/pods"}
	assert.Eventually(t, func() bool { return len(lists()) >= len(want) }, 20*time.Second, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, want, lists())
	assert.False(t, read.Load(), "a cache of it would not sync")
	select {
	case err := <-done:
		t.Fatal("stopped", err)
	default:
	}
	// It comes, and goes, and neither ends the controller.
	for _, now := range []bool{true, false, true} {
		bound.Store(now)
		time.Sleep(100 * time.Millisecond)
	}
	assert.Eventually(t, cached.Load, 20*time.Second, 20*time.Millisecond)
	stop()
	require.NoError(t, first(t, done))
}

func TestController_RunsWithNoNamespaceItMayList(t *testing.T) {
	cfg, lists := api(t, func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/namespaces/team-") })
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- soon("team-a,team-b").run(ctx, cfg, noLog) }()
	assert.Eventually(t, func() bool { return len(lists()) >= 2 }, 20*time.Second, 20*time.Millisecond)
	stop()
	require.NoError(t, first(t, done))
	assert.Equal(t, []string{"GET /api/v1/namespaces/oom/configmaps", "GET /api/v1/namespaces/oom/pods"}, lists())
}

// One whose grant went after it was looked for: it starts over, and looks again.
func TestController_EndsWhereItsCachesDoNotSync(t *testing.T) {
	cfg, _ := api(t, func(r *http.Request) bool {
		return strings.Contains(r.URL.Path, "/namespaces/team-a/") && r.URL.Query().Get("limit") != "1"
	})
	// The manager does not end with the test.
	o := soon("team-a")
	o.syncedWithin = 100 * time.Millisecond
	require.ErrorContains(t, ended(t, func() error { return o.run(context.WithoutCancel(t.Context()), cfg, noLog) }), "have not synced")
}

func TestController_EndsWhereAListIsNotAnswered(t *testing.T) {
	cfg, _ := api(t, func(r *http.Request) bool {
		if r.URL.Query().Get("limit") == "1" {
			<-r.Context().Done()
		}
		return false
	})
	o := soon("team-a")
	o.guardedWithin = 500 * time.Millisecond
	require.ErrorContains(t, ended(t, func() error { return o.run(t.Context(), cfg, noLog) }), "namespace team-a")
}

// What fails otherwise says nothing of a grant.
func TestController_TakesNoOtherFailureForAMissingGrant(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(s.Close)
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: s.URL})
	require.NoError(t, err)
	names := map[string]bool{"team-a": true}
	_, err = unreadable(t.Context(), kube, names)
	require.ErrorContains(t, err, "namespace team-a")
	ctx, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stop()
	assert.False(t, soon("").again(ctx, kube, names))
}

func TestController_WaitsForWhatThePolicySays(t *testing.T) {
	data, err := os.ReadFile("testdata/chart/templates/admission-policies.yaml")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), `message: "`+refused+`"`))
}
