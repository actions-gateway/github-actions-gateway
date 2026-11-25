package cgroup

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v612 is __memory_events_show's layout in Linux v6.12.
const v612 = "low 0\nhigh 0\nmax 41\noom 3\noom_kill 2\noom_group_kill 1\n"

func TestParseEvents(t *testing.T) {
	got, err := parseEvents([]byte(v612))
	require.NoError(t, err)
	assert.Equal(t, Events{OOM: 3, OOMKill: 2, OOMGroupKill: 1}, got)
}

func TestParseEvents_RefusesWhatItCannotCount(t *testing.T) {
	for name, text := range map[string]string{
		"a kernel with no group kill": strings.Replace(v612, "oom_group_kill 1\n", "", 1),
		"a count that is no number":   strings.Replace(v612, "oom 3", "oom max", 1),
		"a field with no count":       strings.Replace(v612, "oom 3", "oom", 1),
		"an empty file":               "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseEvents([]byte(text))
			assert.Error(t, err)
		})
	}
}

func TestLocalEvents(t *testing.T) {
	const line = "35 24 0:30 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate"
	for name, tc := range map[string]struct {
		mountinfo string
		want      bool
	}{
		"ordinary":                        {line + ",memory_recursiveprot\n", false},
		"local":                           {"24 1 8:1 / / rw - ext4 /dev/sda1 rw\n" + line + ",memory_localevents,memory_recursiveprot\n", true},
		"another filesystem's own option": {"24 1 8:1 / / rw - ext4 /dev/sda1 rw,memory_localevents\n" + line + "\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LocalEvents(strings.NewReader(tc.mountinfo))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEventsSince(t *testing.T) {
	for now, want := range map[Events]Events{
		{5, 6, 7}: {1, 1, 1},
		{4, 5, 6}: {},
		{3, 9, 9}: {3, 9, 9},
		{9, 4, 9}: {9, 4, 9},
		{9, 9, 5}: {9, 9, 5},
	} {
		assert.Equal(t, want, now.since(Events{4, 5, 6}), "%+v", now)
	}
}
