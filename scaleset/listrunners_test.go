package scaleset_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// The discovery half of the registration-record sweep (Q550). DeregisterRunnerByName
// can only clear a record whose exact name resolves, which leaves nothing to collect
// records the AGC has forgotten the names of; ListRunnersWithPrefix is what finds them.

func TestClient_ListRunnersWithPrefix(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)

	srv.FailJITConfigName("linux-a")
	srv.FailJITConfigName("linux-b")
	srv.FailJITConfigName("windows-a")
	srv.SetRunnerOnline("linux-b")
	srv.SetRunnerBusy("linux-b")

	got, err := c.ListRunnersWithPrefix(ctx, "linux-")
	if err != nil {
		t.Fatalf("ListRunnersWithPrefix: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListRunnersWithPrefix returned %d records, want 2 (the windows- record must not match): %+v", len(got), got)
	}

	byName := map[string]scaleset.Runner{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if a := byName["linux-a"]; a.Online() || a.Busy || a.ID == 0 {
		t.Fatalf("linux-a = %+v; want an offline, non-busy record with a resolvable id", a)
	}
	// A JIT record is offline until a runner connects, which is why the sweep cannot
	// treat offline alone as sweepable.
	if b := byName["linux-b"]; !b.Online() || !b.Busy {
		t.Fatalf("linux-b = %+v; want online and busy", b)
	}

	// A prefix nothing matches is an empty result, not an error.
	none, err := c.ListRunnersWithPrefix(ctx, "macos-")
	if err != nil || len(none) != 0 {
		t.Fatalf("ListRunnersWithPrefix(macos-) = (%v, %v); want (empty, nil)", none, err)
	}
}

// TestClient_ListRunnersWithPrefixPaginates covers the paging walk: the REST endpoint
// filters only by exact name, so the sweep pages the owner's whole listing and filters
// client-side.
func TestClient_ListRunnersWithPrefixPaginates(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)

	// More than one page of 100, half of them under the prefix.
	const total = 250
	for i := 0; i < total; i++ {
		if i%2 == 0 {
			srv.FailJITConfigName(fmt.Sprintf("linux-%03d", i))
		} else {
			srv.FailJITConfigName(fmt.Sprintf("windows-%03d", i))
		}
	}

	got, err := c.ListRunnersWithPrefix(ctx, "linux-")
	if err != nil {
		t.Fatalf("ListRunnersWithPrefix: %v", err)
	}
	if len(got) != total/2 {
		t.Fatalf("ListRunnersWithPrefix returned %d records across pages, want %d", len(got), total/2)
	}
}

func TestClient_DeregisterRunnerByID(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	ctx := testContext(t)
	c := newClient(t, srv, nil)

	srv.FailJITConfigName("stale")
	srv.FailJITConfigName("busy")
	srv.SetRunnerBusy("busy")

	records, err := c.ListRunnersWithPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListRunnersWithPrefix: %v", err)
	}
	ids := map[string]int64{}
	for _, r := range records {
		ids[r.Name] = r.ID
	}

	if err := c.DeregisterRunnerByID(ctx, ids["stale"], "stale"); err != nil {
		t.Fatalf("DeregisterRunnerByID(stale): %v", err)
	}

	// A record still running a job must surface as *RunnerBusyError so the caller keeps
	// it, exactly as the by-name path reports it.
	err = c.DeregisterRunnerByID(ctx, ids["busy"], "busy")
	var busyErr *scaleset.RunnerBusyError
	if !errors.As(err, &busyErr) {
		t.Fatalf("DeregisterRunnerByID(busy) = %v; want *RunnerBusyError", err)
	}

	left, err := c.ListRunnersWithPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListRunnersWithPrefix after deletes: %v", err)
	}
	if len(left) != 1 || left[0].Name != "busy" {
		t.Fatalf("records left = %+v; want only the busy one", left)
	}
}

func TestClient_ListRunnersWithPrefixErrors(t *testing.T) {
	ctx := testContext(t)

	newRESTClient := func(configURL, apiBase string) *scaleset.Client {
		c, err := scaleset.New(scaleset.Config{
			TokenProvider: fakeProvider{},
			ConfigURL:     configURL,
			APIBase:       apiBase,
			HTTPClient:    http.DefaultClient,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return c
	}

	// Malformed ConfigURL: no org or owner/repo path to derive the runners prefix from.
	if _, err := newRESTClient("https://github.com/", "https://api.github.com").ListRunnersWithPrefix(ctx, "x"); err == nil {
		t.Fatal("ListRunnersWithPrefix with a path-less ConfigURL: want error, got nil")
	}

	// Non-200 from the listing.
	listErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer listErr.Close()
	if _, err := newRESTClient("https://github.com/org", listErr.URL).ListRunnersWithPrefix(ctx, "x"); err == nil {
		t.Fatal("ListRunnersWithPrefix with a 500 response: want error, got nil")
	}

	// Undecodable body.
	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer badJSON.Close()
	if _, err := newRESTClient("https://github.com/org", badJSON.URL).ListRunnersWithPrefix(ctx, "x"); err == nil {
		t.Fatal("ListRunnersWithPrefix with an undecodable body: want error, got nil")
	}

	// A server that reports a total it never pages out must terminate on the short page
	// rather than walking to the page cap.
	pages := 0
	shortPage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pages++
		_, _ = w.Write([]byte(`{"total_count":9999,"runners":[{"id":1,"name":"x-1","status":"offline","busy":false}]}`))
	}))
	defer shortPage.Close()
	got, err := newRESTClient("https://github.com/org", shortPage.URL).ListRunnersWithPrefix(ctx, "x-")
	if err != nil {
		t.Fatalf("ListRunnersWithPrefix against a short page: %v", err)
	}
	if len(got) != 1 || pages != 1 {
		t.Fatalf("got %d records over %d requests; want 1 record from 1 request (a short page ends the walk)", len(got), pages)
	}

	// A server that always returns a full page is bounded by the page cap rather than
	// looping forever.
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":100000,"runners":[` + fullPage(100) + `]}`))
	}))
	defer full.Close()
	capped, err := newRESTClient("https://github.com/org", full.URL).ListRunnersWithPrefix(ctx, "x-")
	if err != nil {
		t.Fatalf("ListRunnersWithPrefix against an endless listing: %v", err)
	}
	if len(capped) != 20*100 {
		t.Fatalf("got %d records; want the page cap to stop the walk at 20 pages of 100", len(capped))
	}
}

// fullPage renders n runner objects for a synthetic full page.
func fullPage(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += `{"id":` + strconv.Itoa(i+1) + `,"name":"x-` + strconv.Itoa(i) + `","status":"offline","busy":false}`
	}
	return out
}
