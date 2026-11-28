package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
)

func uid(n int) string { return fmt.Sprintf("1b4e28ba-2fa1-11d2-883f-%012d", n) }
func id(n int) string  { return fmt.Sprintf("%064d", n) }

func whole(pod, at int, ids ...string) Kill {
	return Kill{Pod: uid(pod), At: int64(at), Rise: &cgroup.Rise{GroupKills: 1, Containers: ids}}
}

func TestName(t *testing.T) {
	node, ok := Node(Name("node-a.example"))
	assert.True(t, ok)
	assert.Equal(t, "node-a.example", node)
	for _, name := range []string{"kube-root-ca.crt", "oom-report-", ""} {
		_, ok := Node(name)
		assert.False(t, ok, name)
	}
}

func TestDecode_ReturnsWhatWasEncoded(t *testing.T) {
	want := []Kill{
		{Pod: uid(1), At: 1_800_000_000_123, Rise: &cgroup.Rise{
			Late: true, Confirmed: true, Torn: true, Recent: true, RecentOwn: true, GroupKills: 1, Singles: 2, Inside: 3, Pod: 4,
			Pods: 5, Own: 6, Inferred: 7, Read: true, Peers: 8, Containers: []string{id(1), id(2)},
		}},
		{Pod: strings.Repeat("a", 32), At: 2, Processes: 3},
	}
	data, err := Encode(want, nil)
	require.NoError(t, err)
	got, _, err := Decode(data)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, int64(1_800_000_000_123), got[0].Time().UnixMilli())
}

func TestDecode_AnEmptyReportHasNone(t *testing.T) {
	got, lines, err := Decode("")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, lines)
}

func line(pod, container int) Line {
	return Line{Pod: uid(pod), Container: id(container), Level: cgroup.PodLevel, At: 1_800_000_000_123}
}

func TestDecode_ReturnsTheLinesEncoded(t *testing.T) {
	want := []Line{line(1, 1), {Pod: uid(2), Container: id(2), Level: cgroup.Above, At: 5}}
	data, err := Encode(nil, want)
	require.NoError(t, err)
	_, got, err := Decode(data)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestDecode_RefusesALineNoReaderSends(t *testing.T) {
	for name, l := range map[string]Line{
		"a UID with no dashes": {Pod: strings.Repeat("a", 32), Container: id(1), Level: cgroup.InPod},
		"no container":         {Pod: uid(1), Level: cgroup.InPod},
		"no level":             {Pod: uid(1), Container: id(1)},
		"a level past Above":   {Pod: uid(1), Container: id(1), Level: cgroup.Above + 1},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := Encode(nil, []Line{l})
			require.NoError(t, err)
			_, _, err = Decode(data)
			assert.Error(t, err)
			_, err = l.MarshalBinary()
			assert.Error(t, err)
		})
	}
	data, err := Encode(nil, make([]Line, MaxLines+1))
	require.NoError(t, err)
	_, _, err = Decode(data)
	assert.ErrorContains(t, err, "lines, over")
}

// A controller older than lines reads the kills of a report that has them.
func TestDecode_AnOlderReaderSkipsLines(t *testing.T) {
	data, err := Encode([]Kill{whole(1, 1, id(1))}, []Line{line(1, 1)})
	require.NoError(t, err)
	var old struct {
		Version int    `json:"version"`
		Kills   []Kill `json:"kills"`
	}
	require.NoError(t, json.Unmarshal([]byte(data), &old))
	assert.Equal(t, 1, old.Version)
	assert.Len(t, old.Kills, 1)
}

func TestLine_Binary(t *testing.T) {
	want := line(7, 9)
	b, err := want.MarshalBinary()
	require.NoError(t, err)
	assert.Len(t, b, LineSize)
	var got Line
	require.NoError(t, got.UnmarshalBinary(b))
	assert.Equal(t, want, got)
	assert.Error(t, got.UnmarshalBinary(b[:LineSize-1]))
	b[48] = 0
	assert.Error(t, got.UnmarshalBinary(b), "a level of 0")
}

func TestLines_KeepsTheNewestOnce(t *testing.T) {
	var l Lines
	assert.True(t, l.Add(line(1, 1)))
	assert.False(t, l.Add(line(1, 1)))
	for i := range MaxLines {
		l.Add(line(2, i+2))
	}
	got := l.Lines()
	assert.Len(t, got, MaxLines)
	assert.NotContains(t, got, line(1, 1))
	assert.Equal(t, line(2, MaxLines+1), got[len(got)-1])
}

func TestDecode_RefusesWhatNoAgentWrites(t *testing.T) {
	rise := `"rise":{"groupKills":1}`
	kill := func(fields string) string { return `{"version":1,"kills":[{` + fields + `}]}` }
	_, _, err := Decode(kill(`"pod":"` + uid(1) + `",` + rise))
	require.NoError(t, err)
	for name, tc := range map[string]struct{ data, want string }{
		"no JSON":                     {"version 1", "not a report"},
		"a newer version":             {`{"version":2,"kills":[]}`, "version 2"},
		"no version":                  {`{"kills":[]}`, "version 0"},
		"too big":                     {`{"version":1}` + strings.Repeat(" ", MaxSize), "bytes"},
		"too many kills":              {`{"version":1,"kills":[` + strings.Repeat("{},", MaxKills) + `{}]}`, "129 kills"},
		"a name for a UID":            {kill(`"pod":"runner",` + rise), "no pod's UID"},
		"neither":                     {kill(`"pod":"` + uid(1) + `"`), "not one of"},
		"both":                        {kill(`"pod":"` + uid(1) + `","processes":1,` + rise), "not one of"},
		"more than a UID":             {kill(`"pod":"` + uid(1) + `-and-more",` + rise), "no pod's UID"},
		"more than a container's ID":  {kill(`"pod":"` + uid(1) + `","rise":{"containers":["` + id(1) + `.scope"]}`), "no container's ID"},
		"a name for a container's ID": {kill(`"pod":"` + uid(1) + `","rise":{"containers":["runner"]}`), "no container's ID"},
		"too many containers": {
			kill(`"pod":"` + uid(1) + `","rise":{"containers":[` + strings.Repeat(`"`+id(1)+`",`, maxContainers) + `"` + id(1) + `"]}`), "17 containers"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Decode(tc.data)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLog_AFullLogIsAReport(t *testing.T) {
	var l Log
	var ids []string
	for i := range maxContainers + 1 {
		ids = append(ids, id(i))
	}
	for i := range MaxKills + 1 {
		k := whole(i, 1_800_000_000_000+i, ids...)
		k.Rise.Read = true
		l.Add(k)
	}
	kills := l.Kills()
	assert.False(t, kills[0].Rise.Read)
	data, err := Encode(kills, nil)
	require.NoError(t, err)
	got, _, err := Decode(data)
	require.NoError(t, err)
	assert.Len(t, got, MaxKills)
}

func TestLog_AFullLogDropsTheOldestOfThePodWithTheMost(t *testing.T) {
	var l Log
	l.Add(whole(1, 0))
	l.Add(whole(1, 1))
	for i := range MaxKills {
		l.Add(whole(2, 100+i))
	}
	kills := l.Kills()
	require.Len(t, kills, MaxKills)
	assert.Equal(t, whole(1, 0), kills[0])
	assert.Equal(t, whole(1, 1), kills[1])
	assert.Equal(t, whole(2, 102), kills[2])
	assert.Equal(t, whole(2, 100+MaxKills-1), kills[MaxKills-1])
}

func TestLog_TakesAPodsLostProcessesForOneAMinute(t *testing.T) {
	stray := func(pod, at int) Kill { return Kill{Pod: uid(pod), At: int64(at), Processes: 1} }
	single := func(at int, ids ...string) Kill {
		return Kill{Pod: uid(1), At: int64(at), Rise: &cgroup.Rise{Singles: 1, Containers: ids}}
	}
	for name, tc := range map[string]struct {
		was, now Kill
		added    bool
	}{
		"soon after":              {stray(1, 0), stray(1, 59_999), false},
		"a minute later":          {stray(1, 0), stray(1, 60_000), true},
		"another pod":             {stray(1, 0), stray(2, 1), true},
		"one container's":         {single(0, id(1)), single(1, id(1)), false},
		"another container's":     {single(0, id(1)), single(1, id(2)), true},
		"one container more":      {single(0, id(1)), single(1, id(1), id(2)), true},
		"a container's after":     {stray(1, 0), single(1, id(1)), true},
		"a whole container's":     {whole(1, 0, id(1)), whole(1, 1, id(1)), true},
		"a process's after that":  {whole(1, 0), stray(1, 1), true},
		"a whole container after": {single(0, id(1)), whole(1, 1, id(1)), true},
	} {
		t.Run(name, func(t *testing.T) {
			var l Log
			assert.True(t, l.Add(tc.was))
			assert.Equal(t, tc.added, l.Add(tc.now))
			want := []Kill{tc.was}
			if tc.added {
				want = append(want, tc.now)
			}
			assert.Equal(t, want, l.Kills())
		})
	}
}
