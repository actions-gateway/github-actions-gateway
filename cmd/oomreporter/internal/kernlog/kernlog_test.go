package kernlog

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The text of kills the kernel printed, from rule/testdata: kind's at a container's limit and
// at its pod's (6.12.76), and GKE's at the pods' limit (6.8.0-1064-gke).
const (
	kindRoot   = "/docker/c4418cb3a46760163d03c4e46b92727c5dd6c8a99d2a85d45fe7130503ad4142/kubelet.slice/kubelet-kubepods.slice"
	kindPod    = kindRoot + "/kubelet-kubepods-burstable.slice/kubelet-kubepods-burstable-podb390f2d1_9e00_498a_9406_187fe7cca0e3.slice"
	kindScope  = "cri-containerd-9e8e1c5841bd2e8b745a74d11402469c0915e08c013244cc5f1cf95f8e190330.scope"
	kindVictim = kindPod + "/" + kindScope

	kindContainer = "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=" + kindScope + ",mems_allowed=0," +
		"oom_memcg=" + kindVictim + ",task_memcg=" + kindVictim + ",task=probe,pid=138360,uid=65534"
	kindPodLimit = "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null)," +
		"cpuset=cri-containerd-5f27bbd70f0017db1e3839ac1308f795a646a599f96f90c072e2e9cdfc20e137.scope,mems_allowed=0," +
		"oom_memcg=" + kindRoot + "/kubelet-kubepods-poddcaeeff0_7395_4639_9ba9_beb68e7514f3.slice," +
		"task_memcg=" + kindRoot + "/kubelet-kubepods-poddcaeeff0_7395_4639_9ba9_beb68e7514f3.slice/" +
		"cri-containerd-5f27bbd70f0017db1e3839ac1308f795a646a599f96f90c072e2e9cdfc20e137.scope,task=probe,pid=138158,uid=65534"
	gkePods = "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null)," +
		"cpuset=cri-containerd-3561b0c85838b3408517503eb759eed0d066dc97dddbf814d5d5d42c3cdd0689.scope,mems_allowed=0," +
		"oom_memcg=/kubepods.slice,task_memcg=/kubepods.slice/kubepods-burstable.slice/" +
		"kubepods-burstable-podd6348d3f_c5a4_4707_95bc_49c91bcbecf9.slice/" +
		"cri-containerd-7eb0a60347196108e7fbeeb2a786ae8530dbf812c62242f524235d100dfd92a9.scope,task=prometheus,pid=7925,uid=1000"
)

// A node-wide kill's line, which no recording here has: dump_oom_victim and
// mem_cgroup_print_oom_context in Linux v6.12 print global_oom for the limit.
const global = "oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=" + kindScope + ",mems_allowed=0," +
	"global_oom,task_memcg=" + kindVictim + ",task=probe,pid=7,uid=65534"

// at is the line a READ_ALL gives for text: a kill is pr_info, so at the kernel's facility
// and priority 6 (record_print_text and info_print_prefix, Linux v6.12).
func at(stamp, text string) string { return "<6>[" + stamp + "] " + text }

func one(t *testing.T, line string) (Kill, error) {
	t.Helper()
	kills, refused := Read(line + "\n")
	require.LessOrEqual(t, len(kills)+len(refused), 1, "one line, one answer")
	if len(refused) == 1 {
		return Kill{}, refused[0]
	}
	require.Len(t, kills, 1, "a kill's line is taken or refused")
	return kills[0], nil
}

func TestRead_TakesTheKernelsLines(t *testing.T) {
	for name, tc := range map[string]struct {
		line         string
		limit, owner string
	}{
		"a container's limit, kind": {at("19161.756446", kindContainer), kindVictim, kindVictim},
		"a pod's limit, kind":       {at("19153.043327", kindPodLimit), kindRoot + "/kubelet-kubepods-poddcaeeff0_7395_4639_9ba9_beb68e7514f3.slice", ""},
		"the pods' limit, GKE":      {at("   12.000001", gkePods), "/kubepods.slice", ""},
		"the node's memory":         {at("19161.756446", global), "", kindVictim},
		"with no stamp":             {"<6>" + kindContainer, kindVictim, kindVictim},
		"with the caller":           {"<6>[19161.756446][  T4242] " + kindContainer, kindVictim, kindVictim},
		"with the caller alone":     {"<6>[  T4242] " + kindContainer, kindVictim, kindVictim},
	} {
		t.Run(name, func(t *testing.T) {
			k, err := one(t, tc.line)
			require.NoError(t, err)
			assert.Equal(t, tc.limit, k.Limit)
			if tc.owner != "" {
				assert.Equal(t, tc.owner, k.Victim)
			}
			assert.True(t, holds(k.Limit, k.Victim))
		})
	}
}

func TestRead_ReadsTheFields(t *testing.T) {
	k, err := one(t, at("19161.756446", kindContainer))
	require.NoError(t, err)
	assert.Equal(t, Kill{
		At: 19161*time.Second + 756446*time.Microsecond, Stamped: true, Constraint: "CONSTRAINT_MEMCG",
		Limit: kindVictim, Victim: kindVictim, Task: "probe", PID: 138360, UID: 65534,
	}, k)
}

// Each line here is the kernel's text with one thing changed, and each is refused for that alone.
func TestRead_RefusesWhatIsNotTheKernelsLine(t *testing.T) {
	victim := ",task_memcg=" + kindVictim
	for name, tc := range map[string]struct {
		line string
		want error
	}{
		// A user's write to /dev/kmsg is moved to LOG_USER (devkmsg_write, Linux v6.12); a kernel message
		// that prints a user's string with a newline in it is at another priority.
		"a user's facility":    {strings.Replace(at("1.000000", kindContainer), "<6>", "<14>", 1), ErrPriority},
		"a warning's priority": {strings.Replace(at("1.000000", kindContainer), "<6>", "<4>", 1), ErrPriority},

		// A cgroup's name may hold a comma and an equals sign: a key in the first name, and
		// a key with a path in a path of the job's own, with nested cgroups named for them.
		"a key in the first name": {at("1.000000", strings.Replace(kindContainer, ".scope,mems", ".scope,task=x,mems", 1)), ErrDuplicate},
		"a key with a path in the victim's": {at("1.000000", strings.Replace(kindContainer, victim,
			victim+"/x,task_memcg="+kindRoot+"/kubelet-kubepods-podother.slice/c", 1)), ErrDuplicate},
		"the node's limit named in the first name": {at("1.000000", strings.Replace(kindContainer, ".scope,mems", ".scope,global_oom,mems", 1)), ErrForm},

		"no limit's cgroup for a cgroup's limit":  {at("1.000000", strings.Replace(global, "CONSTRAINT_NONE", "CONSTRAINT_MEMCG", 1)), ErrConstraint},
		"a cgroup's limit for another constraint": {at("1.000000", strings.Replace(kindContainer, "CONSTRAINT_MEMCG", "CONSTRAINT_NONE", 1)), ErrConstraint},

		// Another CPU between its pieces, or a process name with a newline in it, ends a
		// record early.
		"a line split after mems_allowed":   {at("1.000000", kindContainer[:strings.Index(kindContainer, ",oom_memcg=")]), ErrForm},
		"a line split after the task":       {at("1.000000", kindContainer[:strings.Index(kindContainer, ",pid=")]), ErrForm},
		"a pid that is no number":           {at("1.000000", strings.Replace(kindContainer, "pid=138360", "pid=1x", 1)), ErrForm},
		"text after the uid":                {at("1.000000", kindContainer+" "), ErrForm},
		"the keys out of order":             {at("1.000000", strings.Replace(kindContainer, "nodemask=(null),cpuset="+kindScope, "cpuset="+kindScope+",nodemask=(null)", 1)), ErrForm},
		"a line with no cpuset":             {at("1.000000", strings.Replace(kindContainer, ",cpuset="+kindScope, "", 1)), ErrForm},
		"the constraint's key renamed":      {at("1.000000", strings.Replace(kindContainer, "constraint=", "kconstraint", 1)), ErrForm},
		"a nodemask that is no node list":   {at("1.000000", strings.Replace(kindContainer, "nodemask=(null)", "nodemask=x", 1)), ErrForm},
		"mems_allowed that is no node list": {at("1.000000", strings.Replace(kindContainer, "mems_allowed=0", "mems_allowed=0x", 1)), ErrForm},
		"text after global_oom":             {at("1.000000", strings.Replace(global, "global_oom,", "global_oomx,", 1)), ErrForm},
		"a constraint the kernel has not":   {at("1.000000", strings.Replace(global, "CONSTRAINT_NONE", "CONSTRAINT_X", 1)), ErrForm},
		"a stamp with no space after it":    {"<6>[1.000000]" + kindContainer, nil},

		// pr_cont_kernfs_path prints these where it cannot print the path (Linux v6.12).
		"a victim's name too long":  {at("1.000000", strings.Replace(global, victim, ",task_memcg=(name too long)", 1)), ErrPath},
		"a victim's path of (null)": {at("1.000000", strings.Replace(global, victim, ",task_memcg=(null)", 1)), ErrPath},

		"a victim outside the limit": {at("1.000000", strings.Replace(kindPodLimit, "task_memcg="+kindRoot+"/kubelet-kubepods-poddcaeeff0", "task_memcg="+kindRoot+"/kubelet-kubepods-podother", 1)), ErrOutside},
	} {
		t.Run(name, func(t *testing.T) {
			kills, refused := Read(tc.line + "\n")
			assert.Empty(t, kills)
			if tc.want == nil {
				assert.Empty(t, refused, "not read as a kill's line at all")
				return
			}
			require.Len(t, refused, 1)
			assert.ErrorIs(t, refused[0], tc.want)
		})
	}
}

// A path of the job's own can be as long as it likes, and a piece that does not fit its record
// starts another (vprintk_store, Linux v6.12), so a line can end where the job chose. It is
// false only about the job's own pod.
func TestRead_TakesAFalseLineAboutTheJobsOwnPod(t *testing.T) {
	forged := strings.Replace(kindContainer, ",task=probe,pid=138360,uid=65534", "/x,task=y,pid=1,uid=0", 1)
	k, err := one(t, at("1.000000", forged))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(k.Victim, kindVictim+"/"), "inside the job's own container")
}

func group(path string) string {
	return "<6>[19161.756500] Tasks in " + path + " are going to be killed due to memory.oom.group set"
}

func TestRead_JoinsTheGroupKilledWhole(t *testing.T) {
	other := at("19161.900000", kindPodLimit)
	for name, tc := range map[string]struct {
		log  []string
		want []string
	}{
		"the victim's own":            {[]string{at("19161.756446", kindContainer), "<3>[19161.756480] Memory cgroup out of memory: Killed process 138360 (probe)", group(kindVictim)}, []string{kindVictim}},
		"none":                        {[]string{at("19161.756446", kindContainer)}, []string{""}},
		"one outside the victim":      {[]string{at("19161.756446", gkePods), group("/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podother.slice/c")}, []string{""}},
		"one above the limit":         {[]string{at("19161.756446", kindContainer), group(kindPod)}, []string{""}},
		"one at a warning's priority": {[]string{at("19161.756446", kindContainer), strings.Replace(group(kindVictim), "<6>", "<4>", 1)}, []string{""}},
		"one after the next kill":     {[]string{at("19161.756446", kindContainer), other, group(kindVictim)}, []string{"", ""}},
		"one after a refused kill":    {[]string{at("19161.756446", kindContainer), strings.Replace(other, "<6>", "<4>", 1), group(kindVictim)}, []string{""}},
		"one with no kill before it":  {[]string{group(kindVictim), at("19161.756446", kindContainer)}, []string{""}},
		"one for each of two":         {[]string{at("19161.756446", kindContainer), group(kindVictim), at("19162.000000", global), group(kindVictim)}, []string{kindVictim, kindVictim}},
		"one cut short":               {[]string{at("19161.756446", kindContainer), strings.TrimSuffix(group(kindVictim), " set")}, []string{""}},
		"one with no path":            {[]string{at("19161.756446", global), strings.Replace(group(kindVictim), kindVictim, "(name too long)", 1)}, []string{""}},
		"only the first for one kill": {[]string{at("19161.756446", global), group(kindVictim), group(kindPod)}, []string{kindVictim}},
	} {
		t.Run(name, func(t *testing.T) {
			kills, _ := Read(strings.Join(tc.log, "\n"))
			var got []string
			for _, k := range kills {
				got = append(got, k.Group)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
