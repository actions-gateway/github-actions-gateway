package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fixtureModule is a self-contained module under the repo prefix, so its own
// structs count as in-repo and the walk descends into them. ObjectMeta is a
// local stand-in: a root kind is recognized by the embed's name.
const fixtureModule = "github.com/actions-gateway/github-actions-gateway/fx"

var fixtureFiles = map[string]string{
	"go.mod": "module " + fixtureModule + "\n\ngo 1.26\n",
	"meta/meta.go": `package meta

type ObjectMeta struct{ Name string }
`,
	"v1/types.go": `package v1

import "` + fixtureModule + `/meta"

type Widget struct {
	meta.ObjectMeta ` + "`json:\",inline\"`" + `
	Spec   WidgetSpec   ` + "`json:\"spec\"`" + `
	Status WidgetStatus ` + "`json:\"status\"`" + `
}

type WidgetSpec struct {
	Size      int    ` + "`json:\"size\"`" + `
	OnlyConv  string ` + "`json:\"onlyConv\"`" + `
	OnlySet   string ` + "`json:\"onlySet\"`" + `
	Shared    int    ` + "`json:\"shared\"`" + `
	Ignored   string ` + "`json:\"-\"`" + `
	Nested    Inner  ` + "`json:\"nested\"`" + `
}

type Inner struct {
	Deep int ` + "`json:\"deep\"`" + `
}

type WidgetStatus struct {
	Ready     int   ` + "`json:\"ready\"`" + `
	Seen      int64 ` + "`json:\"seen\"`" + `
	Conds     []int ` + "`json:\"conds\"`" + `
	OnlyRead  int   ` + "`json:\"onlyRead\"`" + `
	Literal   int   ` + "`json:\"literal\"`" + `
	Stamp     Stamp   ` + "`json:\"stamp\"`" + `
	Count     Counter ` + "`json:\"count\"`" + `
}

// Stamp's value receiver cannot write the field it is called on.
type Stamp int

func (s Stamp) IsZero() bool { return s == 0 }

// Counter's pointer receiver can.
type Counter int

func (c *Counter) Inc() { *c++ }
`,
	"v2/types.go": `package v2

import "` + fixtureModule + `/meta"

type Widget struct {
	meta.ObjectMeta ` + "`json:\",inline\"`" + `
	Spec WidgetSpec ` + "`json:\"spec\"`" + `
}

type WidgetSpec struct {
	Size   int ` + "`json:\"size\"`" + `
	Shared int ` + "`json:\"shared\"`" + `
	New    int ` + "`json:\"new\"`" + `
}
`,
	"ctrl/ctrl.go": `package ctrl

import (
	v1 "` + fixtureModule + `/v1"
	v2 "` + fixtureModule + `/v2"
)

func appendTo(p *[]int) { *p = append(*p, 1) }

func Reconcile(w *v1.Widget, x *v2.Widget) int {
	w.Spec.OnlySet = "x"
	w.Status.Ready = w.Spec.Size + w.Spec.Nested.Deep
	w.Status.Seen++
	appendTo(&w.Status.Conds)
	w.Status = v1.WidgetStatus{Literal: 1, Ready: w.Status.Ready}
	_ = w.Status.Stamp.IsZero()
	w.Status.Count.Inc()
	return w.Status.OnlyRead + x.Spec.Shared
}
`,
	"ctrl/conversion.go": `package ctrl

import (
	v1 "` + fixtureModule + `/v1"
	v2 "` + fixtureModule + `/v2"
)

func convert(w *v1.Widget) string { return w.Spec.OnlyConv }

func convertNew(x *v2.Widget) int { return x.Spec.New }
`,
	"ctrl/zz_generated.deepcopy.go": `package ctrl

import v2 "` + fixtureModule + `/v2"

func deepCopy(x *v2.Widget) int { return x.Spec.New }
`,
}

func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range fixtureFiles {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRun(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir := writeFixture(t)
	v1, v2 := fixtureModule+"/v1", fixtureModule+"/v2"

	tests := []struct {
		name     string
		groups   []string
		baseline string
		want     []string
	}{
		{
			name:   "versions judged apart",
			groups: []string{v1, v2},
			want: []string{
				"v1 Widget.spec.onlyConv: spec field nothing reads",
				"v1 Widget.spec.onlySet: spec field nothing reads",
				"v1 Widget.spec.shared: spec field nothing reads",
				"v1 Widget.status.onlyRead: status field nothing writes",
				"v1 Widget.status.stamp: status field nothing writes",
				"v2 Widget.spec.new: spec field nothing reads",
				"v2 Widget.spec.size: spec field nothing reads",
			},
		},
		{
			// v1 reads size and v2 reads shared, so each covers the other's copy;
			// new exists only in v2 and is read only by excluded files.
			name:   "one group shares consumption across versions",
			groups: []string{v1 + "," + v2},
			want: []string{
				"v1 Widget.spec.onlyConv: spec field nothing reads",
				"v1 Widget.spec.onlySet: spec field nothing reads",
				"v1 Widget.status.onlyRead: status field nothing writes",
				"v1 Widget.status.stamp: status field nothing writes",
				"v2 Widget.spec.new: spec field nothing reads",
			},
		},
		{
			name:   "baseline silences a known entry and names stale ones",
			groups: []string{v1 + "," + v2},
			baseline: "# known\n" +
				"v1 Widget.spec.onlyConv  # kept for the migration\n" +
				"v1 Widget.spec.onlySet\n" +
				"v1 Widget.status.onlyRead\n" +
				"v1 Widget.status.stamp\n" +
				"v2 Widget.spec.new\n" +
				"v1,v2 Widget.spec.size\n" +
				"v1 Widget.spec.gone\n",
			want: []string{
				"v1 Widget.spec.gone: baselined but no such field; remove it from the baseline",
				"v1,v2 Widget.spec.size: baselined but now has a consumer; remove it from the baseline",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := ""
			if tt.baseline != "" {
				baseline = filepath.Join(t.TempDir(), "baseline.txt")
				if err := os.WriteFile(baseline, []byte(tt.baseline), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, checked, err := run(dir, baseline, tt.groups)
			if err != nil {
				t.Fatal(err)
			}
			if checked == 0 {
				t.Fatal("checked no fields")
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("findings:\n got  %q\n want %q", got, tt.want)
			}
		})
	}
}

func TestRunRejectsBadGroups(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir := writeFixture(t)
	v1, v2 := fixtureModule+"/v1", fixtureModule+"/v2"
	for _, groups := range [][]string{
		{v1, v2, fixtureModule + "/missing"}, // not in the module
		{v1, v2, fixtureModule + "/ctrl"},    // declares no root kind
		{v1},                                 // v2 declares one and goes unnamed
	} {
		if _, _, err := run(dir, "", groups); err == nil {
			t.Errorf("run(%q): want an error, got none", groups)
		}
	}
}
