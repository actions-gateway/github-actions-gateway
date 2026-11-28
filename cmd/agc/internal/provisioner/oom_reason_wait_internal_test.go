package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// newReasonFixture is newWaitFixture for a kill whose start is recorded, for job "build",
// on a gateway whose AGC acts on reasons.
func newReasonFixture(t *testing.T, change func(*corev1.Pod)) *waitFixture {
	t.Helper()
	f := newWaitFixture(t, func(pod *corev1.Pod) {
		pod.Annotations[AnnotationStartedJobID] = "job-build"
		pod.Annotations[annotationJobName] = "build"
		if change != nil {
			change(pod)
		}
	})
	f.p.OOMKillReasons = OOMKillReasonsAct
	return f
}

// say writes a reporter's condition for container on the live pod, as the reporter does.
func (f *waitFixture) say(t *testing.T, container, reason string) {
	t.Helper()
	require.NoError(t, f.c.Status().Update(context.Background(), saying(f.live(t), container, reason)))
}

func (f *waitFixture) raised(t *testing.T) bool {
	t.Helper()
	return readLedger(t, f.p, f.target).MemoryFloors[failureKey("4242", "build")] != nil
}

// requireWithheldAbove asserts the kill was withheld as above_container, naming words.
func (f *waitFixture) requireWithheldAbove(t *testing.T, words string) {
	t.Helper()
	assert.False(t, f.raised(t), "no limit is raised")
	assert.Empty(t, readLedger(t, f.p, f.target).Pending, "no re-run is parked")
	require.Equal(t, []string{"EvictionRerunWithheld"}, f.target.events)
	assert.Contains(t, f.target.messages[0], "a pod condition says "+words)
	assert.Nil(t, f.p.classBreakers.byKey[classBreakerKey{set: f.target.Key(), cause: recoveryCauseOOM}],
		"a withheld kill must not count toward the breaker")
}

func TestOOMReasonWait_WaitsForTheRunnersReason(t *testing.T) {
	f := newReasonFixture(t, nil)
	f.scan(t)
	f.watch(t)
	f.clock.advance(oomReasonWait - time.Second)
	f.scan(t)
	f.requireNothing(t)

	f.say(t, runnerContainer, "PodsLimit")
	assert.True(t, f.p.OOMReasonEndsWait(f.live(t)))
	f.watch(t)
	f.requireWithheldAbove(t, oomReasonWords["PodsLimit"])
}

// TestOOMReasonWait_EndsOnlyOnTheCountersReasons: a reason the kernel log alone gave, one
// the AGC does not know, or one about another container leaves the wait running, and the
// kill is then handled as with no reason.
func TestOOMReasonWait_EndsOnlyOnTheCountersReasons(t *testing.T) {
	// The kernel-log option's reasons, which have words in the record but end no wait.
	for _, reason := range []string{"LogContainerLimit", "LogPodLimit", "LogPodsLimit", "LogAbovePods"} {
		if _, had := oomReasonWords[reason]; !had {
			oomReasonWords[reason] = "a kernel log line says so"
			t.Cleanup(func() { delete(oomReasonWords, reason) })
		}
	}
	for _, tc := range []struct{ name, container, reason string }{
		{"a reason from the kernel log alone", runnerContainer, "LogPodsLimit"},
		{"a reason it does not know", runnerContainer, "SomebodySaysPodsLimit"},
		{"another container's reason", "dind", "PodsLimit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReasonFixture(t, nil)
			f.say(t, tc.container, tc.reason)
			assert.False(t, f.p.OOMReasonEndsWait(f.live(t)))
			f.watch(t)
			f.scan(t)
			f.requireNothing(t)

			f.clock.advance(oomReasonWait)
			f.scan(t)
			assert.True(t, f.raised(t), "once the wait is over the kill is retried as with no reason")
			require.Contains(t, f.target.events, "MemoryLimitRaisedForRetry")
			for _, m := range f.target.messages {
				assert.NotContains(t, m, "a pod condition says")
			}
		})
	}
}

func TestOOMReasonWait_ByReason(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*corev1.Pod)
		withheld     bool
		says         string
	}{
		{"its own limit is retried, and says so", "ContainerLimit", nil, false, oomReasonWords["ContainerLimit"]},
		{"the pod's limit", "PodLimit", nil, true, oomReasonWords["PodLimit"]},
		{"the pods' limit", "PodsLimit", nil, true, oomReasonWords["PodsLimit"]},
		{"above the pods", "AbovePods", nil, true, oomReasonWords["AbovePods"]},
		{"unknown is retried as with no reason", "Unknown", nil, false, ""},
		{"above the pods, on a set whose profile derives no limit", "AbovePods",
			func(p *corev1.Pod) { delete(p.Annotations, AnnotationSizingProfile) }, true, oomReasonWords["AbovePods"]},
		{"above the pods, on a runner with no limit", "PodsLimit",
			func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Limits = nil }, true, oomReasonWords["PodsLimit"]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReasonFixture(t, func(p *corev1.Pod) {
				if tc.change != nil {
					tc.change(p)
				}
				*p = *saying(p, runnerContainer, tc.reason)
			})
			f.watch(t)
			if tc.withheld {
				f.requireWithheldAbove(t, tc.says)
				assert.Contains(t, f.target.messages[0], "No limit was raised and no automatic re-run was requested")
				return
			}
			assert.True(t, f.raised(t))
			require.Contains(t, f.target.events, "MemoryLimitRaisedForRetry")
			said := strings.Join(f.target.messages, "\n")
			if tc.says == "" {
				assert.NotContains(t, said, "a pod condition says")
				assert.Contains(t, said, "does not say whether it reached that limit")
			} else {
				assert.Contains(t, said, "a pod condition says "+tc.says)
			}
		})
	}
}

// TestOOMReasonWait_RecordChangesNothing: the default waits for nothing and acts on no
// reason.
func TestOOMReasonWait_RecordChangesNothing(t *testing.T) {
	for _, mode := range []OOMKillReasonsMode{"", OOMKillReasonsRecord} {
		f := newReasonFixture(t, func(p *corev1.Pod) { *p = *saying(p, runnerContainer, "PodsLimit") })
		f.p.OOMKillReasons = mode
		assert.False(t, f.p.OOMReasonEndsWait(f.live(t)))
		_, waiting := f.p.awaitingOOMReason(f.live(t), f.clock.now())
		assert.False(t, waiting)
		f.watch(t)
		assert.True(t, f.raised(t), "mode %q", mode)
		for _, m := range f.target.messages {
			assert.NotContains(t, m, "a pod condition says")
		}
	}
}

// TestOOMReasonWait_HoldsAKillItMayNotRetry: a kill the class would not retry is still
// kept for its reason, and looked at again when the wait ends.
func TestOOMReasonWait_HoldsAKillItMayNotRetry(t *testing.T) {
	f := newReasonFixture(t, func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Limits = nil })
	end := f.clock.now()
	until, look, hold := f.p.HoldUnclaimedOOM(f.target, f.live(t), end)
	require.True(t, hold)
	assert.True(t, end.Add(oomReasonWait).Equal(look), "looked at %s", look)
	assert.True(t, until.After(look))

	_, _, hold = f.p.HoldUnclaimedOOM(f.target, f.live(t), end.Add(oomReasonWait))
	assert.False(t, hold, "the wait is over")
	f.p.OOMKillReasons = OOMKillReasonsRecord
	_, _, hold = f.p.HoldUnclaimedOOM(f.target, f.live(t), end)
	assert.False(t, hold, "the default holds no such kill")
}

// TestOOMReasonWait_ARetryableKillIsLookedAtWhenItEnds: one held for its start already is
// looked at again when the reason's wait ends, not only on the start's retry.
func TestOOMReasonWait_ARetryableKillIsLookedAtWhenItEnds(t *testing.T) {
	f := newReasonFixture(t, nil)
	now := f.clock.now().Add(oomReasonWait - time.Second)
	_, look, hold := f.p.HoldUnclaimedOOM(f.target, f.live(t), now)
	require.True(t, hold)
	assert.True(t, f.clock.now().Add(oomReasonWait).Equal(look), "looked at %s", look)
}

// TestOOMReasonWait_AGoneKillWaitsForNothing: no reason can be written on a deleted pod,
// so its kill is handled at once.
func TestOOMReasonWait_AGoneKillWaitsForNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
		event  string
	}{
		{"a kill it retries", nil, "MemoryLimitRaisedForRetry"},
		{"a kill it does not", func(p *corev1.Pod) { delete(p.Annotations, AnnotationSizingProfile) }, "EvictionRerunWithheld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReasonFixture(t, tc.change)
			gone := f.live(t)
			require.NoError(t, f.c.Delete(context.Background(), gone))
			done, err := f.p.SettleDeletedKill(context.Background(), f.target, gone)
			require.NoError(t, err)
			<-done
			assert.Equal(t, []string{tc.event}, f.target.events)
		})
	}
}

func TestParseOOMKillReasonsMode(t *testing.T) {
	for in, want := range map[string]OOMKillReasonsMode{
		"": OOMKillReasonsRecord, "Record": OOMKillReasonsRecord, "Act": OOMKillReasonsAct,
		" act ": OOMKillReasonsAct, "Acting": OOMKillReasonsRecord,
	} {
		assert.Equal(t, want, ParseOOMKillReasonsMode(in), "%q", in)
	}
}
