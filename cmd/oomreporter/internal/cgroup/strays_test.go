package cgroup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStrays(t *testing.T) {
	type step struct {
		after time.Duration
		e     Events
		rose  bool
		want  uint64
	}
	const soon = 10 * time.Millisecond
	for name, steps := range map[string][]step{
		"one process":                   {{soon, Events{}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 1}, {groupLag, Events{OOMKill: 1}, false, 0}},
		"not before its group might be": {{soon, Events{}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag - 1, Events{OOMKill: 1}, false, 0}},
		"two of them":                   {{soon, Events{}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag, Events{OOMKill: 2}, false, 2}},
		"one, then another":             {{soon, Events{}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 1}, {soon, Events{OOMKill: 2}, false, 0}, {groupLag, Events{OOMKill: 2}, false, 1}},
		"a whole cgroup's first":        {{soon, Events{}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {soon, Events{OOMKill: 1, OOMGroupKill: 1}, false, 0}, {soon, Events{OOMKill: 9, OOMGroupKill: 1}, true, 0}, {groupLag, Events{OOMKill: 9, OOMGroupKill: 1}, false, 0}, {groupLag, Events{OOMKill: 9, OOMGroupKill: 1}, false, 0}},
		"the rest of it later":          {{soon, Events{}, false, 0}, {groupLag, Events{OOMKill: 1, OOMGroupKill: 1}, false, 0}, {groupLag - 1, Events{OOMKill: 9, OOMGroupKill: 1}, false, 0}, {groupLag, Events{OOMKill: 9, OOMGroupKill: 1}, false, 0}, {groupLag, Events{OOMKill: 9, OOMGroupKill: 1}, false, 0}},
		"one the tracker returned":      {{soon, Events{}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag, Events{OOMKill: 1}, true, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}},
		"before the pod was seen":       {{soon, Events{OOMKill: 3}, false, 0}, {groupLag, Events{OOMKill: 3}, false, 0}, {groupLag, Events{OOMKill: 3}, false, 0}},
		"a pod made again":              {{soon, Events{OOMKill: 3}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}, {groupLag, Events{OOMKill: 1}, false, 0}},
	} {
		t.Run(name, func(t *testing.T) {
			var strays Strays
			at := start
			for i, st := range steps {
				at = at.Add(st.after)
				rises := map[string]Rise{}
				if st.rose {
					rises[burstableUID] = Rise{}
				}
				assert.Equal(t, st.want, strays.Observe(one(Pod{Subtree: st.e}), rises, at)[burstableUID], "step %d", i)
			}
		})
	}
}

func TestStrays_ForgetsAPodThatGoes(t *testing.T) {
	var strays Strays
	strays.Observe(one(Pod{}), nil, start)
	strays.Observe(Sample{}, nil, start)
	assert.Empty(t, strays.pods)
}
