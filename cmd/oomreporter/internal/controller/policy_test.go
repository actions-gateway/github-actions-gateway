package controller

import (
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

// The chart is outside this module, and the symlink keeps it an input of the test cache.
const policies = "testdata/chart-templates/admission-policies.yaml"

// quoted returns what is in quotes, of either kind, after the first match of start on its line.
func quoted(t *testing.T, start string) (all []string) {
	t.Helper()
	data, err := os.ReadFile(policies)
	require.NoError(t, err)
	line := regexp.MustCompile(regexp.QuoteMeta(start) + `(.*)`).FindSubmatch(data)
	require.NotNil(t, line, start)
	for _, m := range regexp.MustCompile(`["']([\w-]+)["']`).FindAllSubmatch(line[1], -1) {
		all = append(all, string(m[1]))
	}
	return all
}

func fields(of reflect.Type) (names []string) {
	for i := range of.NumField() {
		name, _, _ := strings.Cut(of.Field(i).Tag.Get("json"), ",")
		names = append(names, name)
	}
	return names
}

// A field that a later API adds would be the controller's to change.
func TestPolicies_NameEveryFieldOfAPodTheControllerLeaves(t *testing.T) {
	for list, want := range map[string][]string{
		// Every write has its own of those two.
		"$metadata := list": slices.DeleteFunc(fields(reflect.TypeFor[metav1.ObjectMeta]()), func(f string) bool { return f == "resourceVersion" || f == "managedFields" }),
		"$status := list":   slices.DeleteFunc(fields(reflect.TypeFor[corev1.PodStatus]()), func(f string) bool { return f == "conditions" }),
	} {
		assert.ElementsMatch(t, want, quoted(t, list))
	}
}

func TestPolicies_NameEveryReasonOfAnEvent(t *testing.T) {
	want := []string{reasonProcess}
	for r := range maps.Keys(words) {
		want = append(want, eventPrefix+string(r))
	}
	assert.ElementsMatch(t, want, slices.DeleteFunc(quoted(t, "object.?reason.orValue('') in ["), func(s string) bool { return !strings.HasPrefix(s, eventPrefix) }))
}

func TestPolicies_LeaveRoomForTheLongestConditionAndReport(t *testing.T) {
	data, err := os.ReadFile(policies)
	require.NoError(t, err)
	most := func(of string) int {
		m := regexp.MustCompile(regexp.QuoteMeta(of) + ` <= (\d+)`).FindSubmatch(data)
		require.NotNil(t, m, of)
		n, err := strconv.Atoi(string(m[1]))
		require.NoError(t, err)
		return n
	}
	assert.Equal(t, report.MaxSize, most("object.data[k].size()"))
	v := verdict{ended: ended{name: strings.Repeat("a", 63), id: "containerd://" + strings.Repeat("a", 64), at: killedAt}, none: true}
	for v.reason = range words {
		c, _ := v.condition(pod(), killedAt)
		assert.LessOrEqual(t, len(c.Reason), most("c.?reason.orValue('').size()"))
		// Another runtime's IDs may be longer.
		assert.LessOrEqual(t, len(c.Message)+100, most("c.?message.orValue('').size()"))
	}
}

func TestPolicies_AreOfTheNamesTheCodeHas(t *testing.T) {
	data, err := os.ReadFile(policies)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(data), "'"+report.Name("")+"'"))
	assert.Equal(t, 1, strings.Count(string(data), "'"+Domain+"/'"))
}
