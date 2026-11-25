# A node reporter for out-of-memory kills: Q1403

> **Status (2026-11-24): decided, D1 to D6 each as recommended.
> [PR 1](#build-size), the reader and the rule, is built; nothing here has run in the shape proposed.**

A container killed for memory reads `OOMKilled` with exit code 137 whichever limit ran out: its own, its pod's, the one all pods share, or the node's memory.
The AGC therefore names no cause (Q1400), and neither does anything else in the Kubernetes API that a tenant's operator can read on the pod.
This is the design of a small tool that says which limit ran out, on the pod, for anyone debugging such a kill, and of what the Actions Gateway Controller (AGC) would do with the answer.

**How claims are marked.** *(read)* is source at a pinned version: Kubernetes `v1.35.5`, Linux `v6.12`, or this repository at `274ca688f`, where the functions cited were compared with an earlier reading.
*(measured)* names where; *(reported)* is a reading another session took for this design, none of it re-run here and none on GKE, all on kernel 6.12.76: eighteen kills in six runs in one local container with its own `kubepods.slice` and no kubelet, recorded in [Q1316's plan](q1316-dogfood-reproduction.md).
*(recall)* is neither.
A GKE node of Q1316's run is on `6.8.0-1064-gke` (reported).
Of the kernel functions cited here for the counters, the kill and its line, each is the same at `v6.8`, but for the cgroup v1 half of `mem_cgroup_oom` and an argument in `memory_max_write`; `check_syslog_permissions` also takes `CAP_SYS_ADMIN` there (read, compared function by function, without `___ratelimit`, `cgroup_mkdir` and what else prints to the log, and not in Ubuntu's own tree).
[What to measure first](#what-to-measure-first) lists the ones the mechanism rests on; D2 rests on reading alone: what a token can list, and what an escaped job already holds.
A sentence about the proposal itself carries no mark.

## Decisions

| # | Decision | Decided | What it costs |
|---|---|---|---|
| D1 | What it reads | Cgroup counters, by a process that is not root and not privileged; and the kernel log beside them as [an option](#the-kernel-log-as-an-option), off by default | Off: some kills come back `Unknown`, how many is not measured. On: a container per node at uid 0 with `CAP_SYSLOG`, and PR 9 |
| D2 | One component or two | A node agent that cannot read or change pods, and one controller that can | One more image mode and about one more PR than a single DaemonSet; and a node label a kubelet may not set, which is not read to be possible on an autoscaled GKE pool |
| D3 | What the AGC does on a kill made above the pod | No raise and no re-run: the Event says what ran out, and the operator re-runs by hand. No re-run class in this design; it is [a follow-up after 2.1](#the-re-run-class-a-follow-up) | Such a job needs a manual re-run, where today it gets one by accident with a raise attached |
| D4 | On by default? | No: its own chart, not installed by the gateway's, and an AGC setting, off, for acting on the answer. The record of it (PR 6) needs no setting, [within limits](#reading-it) | Operators who do not install it keep Q1400's text. Where anything else writes the condition, a failed job's record repeats it, attributed, on a kill the kubelet reported |
| D5 | Where it lives | This repository: its own module, image and chart, importing nothing of the gateway's | A general tool under a gateway's release cadence |
| D6 | What gates 2.1 | The tool and the AGC's record of its answer, [six PRs](#the-smallest-set-for-21), and reading 8 on GKE. The AGC acting on it (D3) rides 2.1 where it is ready and can follow | Until the AGC acts on it, it still raises on a kill made above the pod, beside a record that says so |

## What it reads

### Two sources

| | Cgroup v2 counters | Kernel log |
|---|---|---|
| What | `memory.events` and `memory.events.local` of each pod, its containers, the QoS slices and the pods' root (`kubepods.slice`) | The `oom-kill:constraint=` line, which names the limit's cgroup (`oom_memcg`, or `global_oom`) and the victim's (`task_memcg`) |
| Says which limit | By inference from which counter rose | Outright |
| Access | `/sys/fs/cgroup` by `hostPath`, read-only | `syslog(2)`, with `CAP_SYSLOG` and no mount or device ([below](#the-kernel-log-as-an-option)); `/dev/kmsg`, which refused uid 65534 with `dmesg_restrict` 1 and opened as root (reported), and is expected to need a privileged container as a device (recall); or the journal's files, with a group that can read every system log on the node (recall) |
| Privilege | Not root, no capabilities, not privileged: uid 65534 opened all three files in each of eight cgroups, and `memory.events` is root's and mode 0444 (reported); not on GKE | uid 0 and one capability by `syslog(2)`; privileged, or the node's whole system log, by the others |
| Discloses, if the reader is taken over | Pod and container identifiers, and how much memory each uses | Every kernel message, or every system service's log |
| Survives a missed moment | The pod's counters outlive the container's cgroup (measured, local 6.12.76), and go with the pod's | Until the ring buffer wraps (recall); the line itself is rate-limited, so a stampede loses some (read, `oom_kill.c`), where the counters lose none |

**Counters (D1).** The kernel log names the limit outright and is a worse thing to hand a process on every node.
It stays the reference the counters are checked against, in the dogfood run and in kind, by a container that is privileged already.

Pod Security's `baseline` forbids `hostPath`, so the agent's namespace is `privileged` level whichever source it reads (read, `docs/design/05-security.md`).
What the counters buy is the container's own privilege, not the namespace's label.

### The kernel log, as an option

Off by default, behind its own value in the reporter's chart.
Nothing else in this design depends on it.

**Are the counters good enough without it?** For a kill the agent was watching, they are expected to be, and the check so far is eighteen kills in a local container, where they and the kernel's line agreed every time (reported).
How often they would answer `Unknown` on a real node is not measured.
Reading 8 compares the two on GKE, and if they differ there, D1 is open again and the log is the better source.

The log is not more complete, it is complete about different things:

| Where | Counters | With the log |
|---|---|---|
| The agent started after the kill, or missed it | `Unknown` | Named, while the line is still in the kernel's buffer and the pod is still in the API |
| Two kills in one sample | `Unknown` | Each line names its own limit and its own victim |
| Which of two containers it was | `Unknown` | Named: `task_memcg` is the victim's cgroup |
| The pod's cgroup went before it was read | Nothing | Named, as the first row |
| A `memory_localevents` node | Nothing | Named, by the log alone |
| A `singleProcessOOMKill` node | `Unknown` | Still `Unknown`: the line names a process, and not whether its container ended by it |
| `AbovePods` | One reason for the node's memory and for a slice above the pods | The same reason, and the message says which: a slice, all of the node's memory, or one set of its memory nodes, which the line's `constraint` tells apart (read, `constrained_alloc`) |
| More than ten kills on the node in five seconds | Unaffected | The rest have no line. The limit is one for the whole node, so a job that loops can drop a neighbour's, and a line is dropped too where two kills contend for it (read, `oom_kill_process` and `___ratelimit`) |
| Another CPU prints between the pieces of a line | Unaffected | That line is refused, as a split one: a piece joins its line only where the last record is the same caller's (read, `vprintk_store`). How often is not measured |
| A kill in a Kata guest | Nothing | Nothing: it is the guest's log |

How long a line lasts is not measured: on kind the buffer still held everything since the virtual machine's boot, 4,891 lines, with nothing lost.
A node-wide kill prints every process on the node first (read, `dump_tasks`), so a few of them on a crowded node may push older lines out.

**Two witnesses, one answer.**

| Counters | Log | Reason written |
|---|---|---|
| A reason | The same, or no line | The counters' |
| A reason | Another | The counters'; the message and the Event say what the line names |
| `Unknown`, or nothing | A reason | `LogContainerLimit`, `LogPodLimit`, `LogPodsLimit` or `LogAbovePods` |
| `Unknown`, or nothing | No line | As with the option off |

The log adds words and never takes an answer away.
`Unknown` in the second row would: PR 7 raises and re-runs on it, so a line a job forged about its own pod would give it back the raise D3 withholds.
The third row is where the option adds an answer, and it has names of its own so that a reader can tell.
**The AGC never acts on a `Log` reason.** To PR 7 it is no condition at all, and it does not end [the wait](#timing), since a forged line can be there before the counters' answer.
A counters' reason replaces a `Log` one for the same container ID, and a `Log` one replaces nothing but `Unknown`.
PR 6's record can say it, with four more sets of words.
A reader that does not know the names takes them as `Unknown`, as it does any other, and PR 7's wait ends on the counters' five names alone.

**Both witnesses go through [the rule](#the-rule).** The kernel names a cgroup, and the limit it stands for is a question for the pod's spec: for every one-container worker it is expected to name the pod's slice (read, `ResourceConfigForPod`), which is `ContainerLimit` there and `PodLimit` only where the spec sets one.
So the reader sends the cgroup's level (in the pod, the pod's own, the QoS slice or the pods' root, above them), and the controller applies the same rows to it.
The container must be the one the line's `task_memcg` is in, `OOMKilled` with exit 137, and named by the kernel's `Tasks in … are going to be killed due to memory.oom.group set`, which is outside the rate limit (read, `mem_cgroup_print_oom_group`), so a nested kill writes no `Log` reason either.

**How it reads.** A second container in the agent's pod: uid 0, every capability dropped but `SYSLOG`, `allowPrivilegeEscalation: false`, `RuntimeDefault` seccomp, no `hostPath`, no device, not privileged, and the pod's `shareProcessNamespace` off.

- `SYSLOG_ACTION_READ_ALL` needs `CAP_SYSLOG` where `dmesg_restrict` is 1, and no capability where it is 0 (read, `check_syslog_permissions`).
- containerd's default seccomp profile allows the call where the container has that capability (read, containerd v2.1.0 `seccomp_default.go`).
- The capability must be held in the host's user namespace (read, `capable`), so not under `hostUsers: false`.
- A security module can refuse the call as well (read, `check_syslog_permissions`), and the option then reports nothing.
- uid 0, because an unprivileged uid keeps a capability over `exec` only from an ambient set or a file's, and `no_new_privs` takes the file's back (read, `cap_bprm_creds_from_file`); that Kubernetes sets no ambient one is recall.
- `READ_ALL` takes no line from anyone, where `SYSLOG_ACTION_READ` shares one position with every `/proc/kmsg` reader (read, `syslog_seq`) and is not used.

It has no service-account token: the pod mounts none by default, and the agent's is projected into the agent's container alone.
It hands the agent numbers over a socket the two share, a pod UID, a container ID, the level of the cgroup that ran out, a time, and never text.
The report marks them as the log's, so a taken-over reader can make a `Log` reason, or a sentence beside a counters' reason, for its own node's pods, and take neither away.
It reads at start, when a counter rises, and every few seconds, which is what finds a kill no counter showed.

`/dev/kmsg` would be the better stream: it numbers its records, so a lost one shows, and escapes a newline inside one, where `syslog(2)` gives neither (read, `msg_add_ext_text` and `record_print_text`).
It costs a privileged container (recall), and the agent's counters already notice a kill the log lost.

**What it reaches, taken over.** The whole kernel log, which holds the name and size of every process in each kill's dump, any tenant's.
It can clear that log for later `syslog(2)` readers, take lines from a `/proc/kmsg` reader, and turn console logging off (read, `do_syslog`).
Where `kptr_restrict` is 1 or 0, `/proc/kallsyms` shows it the kernel's addresses (read, `kallsyms_show_value`), which makes a kernel exploit easier to write.
It holds no credential, sees no pod's memory, and has the pod's network, the API server alone.
It is the host's uid 0, where the agent is not, so a fault in the container runtime or the kernel that lets it out leaves it root on the node.
A cluster policy that refuses added capabilities refuses the option.

**Who can falsify a line.** A process that writes to `/dev/kmsg` cannot claim the kernel's facility (read, `devkmsg_write`), and `syslog(2)` prints the facility before each line, so any other is dropped.
Two ways are left:

- **By name.** The line prints, raw and in this order, the cgroup name of the task that was allocating, the limit's path, the victim's path, and the victim's process name (read, `dump_oom_victim` and the three it calls).
  A cgroup's name may hold a comma and an equals sign, and not a newline (read, `cgroup_mkdir`) or a slash.
  So a job that can make nested cgroups can put a key in the first name, with no path after it, and a key with a path in either of its own paths.
  It cannot remove the real fields, but a path can be as long as it likes, and a piece that does not fit its record starts another (read, `vprintk_store`), so a line can be made to end where the job chose and look whole.
  It cannot change how a path of its own starts: with its pod, or, for an unsandboxed `privileged-dind` job with `--cgroup-parent`, with whatever slice it named.
  **The reader matches the line against its full form, and refuses one**
  - whose prefix is not the kernel's facility at the priority the kernel prints a kill with, `<6>`;
  - where a key appears twice, or `global_oom` and `oom_memcg` both do;
  - where `constraint=CONSTRAINT_MEMCG` and an `oom_memcg` do not come together (read, `constrained_alloc`);
  - that does not end in `,task=…,pid=…,uid=…`, or prints a path as `(name too long)` or without a leading `/`;
  - whose victim is not inside the cgroup whose limit it names, which is where the kernel takes a victim from.

  A refused line is no line, so the counters stand, and each refusal has a fixture.
  What is left is a false line about the job's own pod, or from such a privileged job about any pod.
  The first name is the allocating task's and not the victim's, so a job can also blank a neighbour's line where its own task was the one allocating.
  That name can also start a record, where another CPU prints between the pieces, and begin with the byte and digit that set a record's priority (read, `vprintk_store` and `printk_parse_prefix`).
  The priority check does not cover that, and the leading `/` does, since a name holds none.
- **By newline.** `syslog(2)` gives each line of a record its own prefix and escapes nothing (read, `record_print_text`), so a kernel message that prints a user's string with a newline in it would start a line that looks whole.
  A job can reach one such message: running a file that asks for an executable stack prints four components of its path, unescaped, once a boot (read, `setup_arg_pages` and `dentry_name`).
  It prints at priority 4, so the priority check refuses it.
  A segfault prints the process's name, 15 bytes, and one component of a path at priority 6 (read, `show_signal_msg`, x86), and a component holds no `/`, so the leading `/` refuses that.
  `logfc` prints a mount's error text unescaped too (read); the kernel is expected to have split that text at commas first (recall).
  No full search was made for others.
  A privileged job can print what it likes (a module, reasoned).

So a wholly false line is not excluded, and that is the reason for the rule above: beside a counters' reason it adds a sentence, and alone it makes a `Log` reason, which nothing acts on.

### The rule

Two facts about the counters carry it, both read (`mm/memcontrol.c`, and `memcg_memory_event` in `include/linux/memcontrol.h`) and both true of every kill reported:

- `oom` is raised on the cgroup that refused the charge, before anything is killed, and then on its ancestors.
- `.local` counts only a cgroup's own.

And one about the kubelet (read, `ResourceConfigForPod`): **it caps the pod's own cgroup at the sum of its containers' limits** where the pod is `Guaranteed`, or where every container has a memory limit, with no pod-level limit in the spec.
For a one-container pod that cap is the container's limit, and the pause container's memory is charged to it too, so the pod's cap goes first (measured on kind, `Burstable` and `Guaranteed`).
Under a pod-level limit, a container with no limit of its own is capped at the pod's (read, `getMemoryLimit`); the kernel named the pod's cgroup there too (measured on kind, both classes).
So the pod's `.local` alone does not mean a pod-level limit, and only the pod's spec says which it is.

The agent therefore sends numbers, and the controller, which has the spec, applies this when a pod's `oom_group_kill` rises:

| Rose with it | Reason written | Means |
|---|---|---|
| The killed container's own `.local` `oom`, or an `oom` inside the pod where that file had gone ([below](#the-container-that-has-gone)) | `ContainerLimit` | Its own limit ran out |
| The pod's `.local` `oom`, and `spec.resources` sets no memory limit | `ContainerLimit` | The containers' limits together, which for one container is its own |
| The pod's `.local` `oom`, and `spec.resources` sets one | `PodLimit` | The pod-level limit |
| Neither; `.local` `oom` of its QoS slice or the pods' root | `PodsLimit` | The limit all pods share: the node's capacity less its reservations, a little above allocatable (read, `getNodeAllocatableInternalAbsolute`) |
| None of these, and no `oom` inside the pod that the kill could be for | `AbovePods` | Something above the pods; on an ordinary node, the node's memory |
| More than one, or a first read later than the kill | `Unknown` | Nothing is claimed |

Readings behind it (kind's measured for this design, the others reported):

| Limit that ran out | Container `.local` `oom` | Pod `.local` `oom` | QoS slice, then `kubepods`, `.local` `oom` | The kernel's `oom_memcg` |
|---|---|---|---|---|
| The container's, under a roomier pod cgroup | 1 | 0 | 0, 0 | The container |
| The container's, in a one-container pod, on kind | 0 | 1 | 0, 0 | The pod |
| The container's, one of two, on kind | Gone before it was read in two of three; 1 in the third | 0, and 1 inside it | 0, 0 | The container |
| The pod's | 0 | 1 | 0, 0 | The pod |
| `kubepods`' | 0 | 0 | 0, 1 | `kubepods` |
| `kubepods`', on kind | 0 | 0 | 0, and 2 or 3 for each of three kills | `kubepods` |
| A slice above `kubepods`, on kind | 0 | 0 | 0, 0 | That slice |

The last is one reading, of a limit put on the slice above the pods' root for a few seconds.
Nothing has filled a node: Q1316's R2 lost it, and [its second run](q1316-dogfood-reproduction.md#the-second-run) drops that fill and reads a kill at `kubepods`' limit.
Its R8 cannot show the second row, because `privileged-dind`'s pod cap has room above the runner's limit (computed, not run).

**Which container.** A pod-level rise does not say, and containerd marks a container `OOMKilled` for an earlier nested kill too (measured on local nodes, Q1395), so a status alone would give a later SIGKILL the nested kill's answer.
Two things narrow it:

- **`oom_group_kill`, not `oom_kill`, is what the condition needs.** It rises where the kernel kills a whole cgroup that asks for it, looked for from the victim up to the limit that ran out and no further (read, `mem_cgroup_get_oom_group`), and the kubelet asks for it on a container unless `singleProcessOOMKill` is set (read).
  So a nested kill with no such setting of its own raises `oom_kill` alone, and a container's own death raises both (measured on kind).
  **The setting is read for each container, and a container without it is `Unknown`.** It is not the same for every container of a node: on GKE every container started before containerd's first restart on a new node read 0, and after it 1 (reported; kubelet v1.35.8-gke.1796000, containerd 2.2.7; the cause was not read).
  A `singleProcessOOMKill` node is the same case.
- **The container is the one whose own `.local` `oom_group_kill` was seen to rise**, or, where its cgroup went unread, the only one whose status ended `OOMKilled` with exit 137 within a bound of the rise, which reading 3 sets.
  Otherwise `Unknown`.
  A container that ended with 137 and another reason counts among those it may be of, and nothing is written on it.

A nested group that does ask for it, such as a nested kind node inside a job, can still be taken for the container, and only beside a container that died of SIGKILL at the same time.

Twelve constraints keep the inference from claiming too much:

- **A rise in the pod's host-side count is required.** `AbovePods` is reached by elimination, so without this a kill the node never made would read as the node's.
  Under Kata the kill happens in the guest (recall), where every host counter stays still; that kill gets no condition.
- **`AbovePods` is not called "node memory".** A limit on a slice above the pods' root would read the same.
  The kubelet's own `SystemOOM` Event on the Node corroborates it: that Event is raised only where the parsed line gave the limit's cgroup as `/`, which is the parser's value for a line with no `oom_memcg` (read, `oom_watcher_linux.go` and cAdvisor's `oomparser.go`).
- **A zero is conclusive without a baseline; a one is not.** The counters are cumulative from the pod's start, so a pod `oom` of 0 says no limit of it or inside it ever ran out, whenever the agent started, and not which one above it did.
  A 1 may be an earlier nested kill, so only a rise across the kill counts.
  For an agent that started after a kill the answer is `Unknown`, and the message carries the zero.
- **The victim's files are read before its ancestors', and both reasons above the pod need the sample after the one that shows the kill.** `oom` rises before `oom_kill`, and one file prints them in that order too (read, `__memory_events_show`), so the other order, or one read that straddles the kill, sees a kill with no limit beside it.
  What stands above the pod is taken from the first of the two samples, since more by the second is a neighbour's.
- **A pod's counts are taken from before an allocation that may still kill.** The kernel prints its report between the allocation and the kill, so a sample can fall between them, and counts that started there would miss the limit.
  So the agent keeps the earlier sample until the kill shows, or for 5 seconds (a guess, for reading 3 to set).
  A kill with no limit beside it, within a minute of one given up on, is `Unknown` and not `AbovePods`.
  So is one at the pod's limit or the pods', within a minute of an allocation left out that may have met the pod's own limit or a container's: that container's kill may be this one.
  The allocation behind a kill already answered is left out too, so a pod's second kill within a minute, at its own or the pods' limit, is `Unknown`.
  A worker's runner does not restart, so this costs a sidecar's kill and other pods'.
  A nested limit's allocation is not such a one where every container's file was there to read.
- **A limit is named only where it has as many `oom` as there were kills to account for**, other pods' included for the limit all pods share, since each whole kill at a limit follows at least one there (read, `mem_cgroup_oom`).
- **A limit below a container is not the container's.** It raises the pod's `oom` and kills no container whole, since the kernel looks for a group to kill no further up than the limit (read, `mem_cgroup_get_oom_group`).
  So the pod's `oom` from inside it accounts for nothing once the killed containers' own files were read: those do.
- **A node whose cgroup tree is mounted `memory_localevents` is refused.** No event then reaches an ancestor (read, `memcg_memory_event`), so a container's kill raises neither the pod's `oom` nor its `oom_group_kill`, and would go unseen.
  The agent reads the mount's options and reports nothing on such a node.
- **A pod's counts start again when it can be read again.** Counts from before a time its files were refused would span all of it, and a neighbour's spare `oom` at the pods' limit with them.
  So what happened while it was shut is `Unknown`, and the next kill is counted from the first sample after (PR 2's; PR 1 keeps the older base).
- **A pod with two cgroup directories is not judged.** The kubelet makes one for a UID, and a job with the host's view of the tree can make another in a different class, with a cgroup named as the victim's container in it.
  Read in its place, that copy's counts would name any limit it chose.
  The copy can outlive the pod's own, so a pod that has had two is not judged again while any directory of its UID is there, and one whose directory went is not judged when another comes within 15 seconds (PR 2's).
  Nor is one whose directory another took the place of between two samples, which its inode tells (that the kernel does not soon use one again is recall).
  An agent that restarts forgets all three.
- **A kill counts for a container only within 5 seconds before to 6 after its end** (the bound is a guess, for reading 3 to set), and two kills of one container with different limits are `Unknown`.
  Once the kubelet has removed the pod's own directory, this is what keeps a copy the agent has no memory against from a later say.
- **An answer that rests on the time of a container's end is held for 10 seconds after it** (PR 2's).
  The kill may be another container's, and that of this one in the node's next report, which is written at most 8 seconds after the one before.
  The times are the node's and the controller's: a controller ahead of the node holds for less, and one over 10 seconds behind it not at all.
  Where a container's own counts show the kill, the answer waits for nothing.
- **A limit lowered below use raises `oom` with no charge failing** (read, `memory_max_write`), as an in-place resize can; the kill that follows is that limit's, and reads so.

#### The container that has gone

Where a pod keeps running beside the container killed at its own limit, that container's cgroup was gone 5 to 20 ms after the kill: of three kills, a reader on inotify read the file in one and a 50 ms poll in none (measured on kind).
A rule that needed its file would answer `Unknown` for most kills at a container's own limit.
So an `oom` inside the pod is also taken as the killed container's where five things hold:

- one whole kill, and no container's file that shows it;
- one container gone since the count began, and no more;
- more `oom` inside the pod than the containers still there account for;
- no container of the pod ever sampled with a cgroup below it, which is `cgroup.stat`'s `nr_descendants` and `nr_dying_descendants` (read, `cgroup_stat_show`; not measured);
- no cgroup of the pod ever sampled that is not named as the runtime names a container's under that node's cgroup driver, which is what a container in [another tenant's job](#who-could-forge-the-answer-r9) can put there.

The last two keep a nested limit from being taken for the container's.
CRI-O can put a container's monitor beside it, named `crio-conmon-` (recall), and that name is let through.
A name alone is not proof: under cgroupfs a Docker container's cgroup has the same bare ID as the runtime's, and a job that can write the host's cgroup tree can use any name.
The controller has the pod's status, so it counts the containers the agent reported against the one sandbox and those the status has running at the kill, and takes a pod with more as nested (PR 2's).
One that ended over 6 seconds before, or started in the kill's second or later, is not counted, since it would make room for another's cgroup.
An ordinary runner cannot make a cgroup (recall), unless containerd's `CgroupWritable` is on (reported, v2.3.1).
`privileged-dind`'s are outside its pod ([above](#what-it-covers)), so its pod does not count as nested.

#### Where it names the wrong limit

A sample cannot tell an allocation that waits for its own kill from one behind the kill it sees, and one kill can have several (2 or 3 at the pods' limit, for each of three kills on kind).
Each shape below needs a second limit to run out at the same moment, or a spare count left from a moment before.

| What happened | Named | What PR 7 does | What is right |
|---|---|---|---|
| Killed above the pods, while another pod's allocation at the pods' limit waits for its own kill, or is spare | `PodsLimit` | No raise, no re-run | The same |
| The same, and the other pod's directory went between two samples | `PodsLimit` | No raise, no re-run | The same |
| Killed above the pod, while another container's allocation at the pod's own limit waits, and a limit in its spec | `PodLimit` | No raise, no re-run | The same |
| The same, and no pod-level limit in its spec | `ContainerLimit` | **Raise and re-run, as today** | No raise |
| Killed above the pod, while an allocation at the same container's own limit waits | `ContainerLimit` | **Raise and re-run, as today** | No raise |
| Killed at its own limit more than a minute after the allocation, with one at its pod's or the pods' limit beside it | `PodLimit` or `PodsLimit` | **No raise, no re-run** | Raise and re-run |
| Killed above the pod, its file gone, just after a limit ran out in a cgroup of the pod, its own or one another tenant's job put there, that came and went between two samples | `ContainerLimit` | **Raise and re-run, as today** | No raise |
| Killed in a way the agent did not see, and within 6 seconds the kubelet removes the pod's cgroups, the agent restarts, and a job with the host's view of the tree has a kill in a copy of them | Whichever limit it filled | Follows that | Nothing to go on (reasoned, not run) |
| Killed at its own limit within 6 seconds of another container's kill above the pod, whose end the status no longer names, and reported more than 10 seconds after its own end | `AbovePods` | **No raise, no re-run** | Raise and re-run |
| Killed at its own limit, its `oom` raised between the read of the container's file and the read of the pod's in one sample, its file then gone, with one at the pods' limit beside it | `PodsLimit` | **No raise, no re-run** | Raise and re-run |

So of the shapes known, four can withhold a raise and a re-run that were right.
One needs an allocation to wait more than a minute for its kill, one a sample to straddle it by the time between two reads, expected to be microseconds (read, not measured), one a job that forges cgroups as the agent restarts, in the seconds after the pod's are removed, and one a container that ended twice in those seconds beside a report that is late.
The others at worst leave a raise that was not right, which is what the AGC does now.
Other orders of two limits were tried against the rule on fakes and read the right limit or `Unknown`; none was tried on a kernel.
A re-run class that trusted `PodsLimit` would have to answer the first two ([Q1408](../queue/Q1408.md)).

### Watching

The agent would keep the last sample of each pod in memory and take a new one where an inotify event for the pod's `memory.events` finds its `oom` or a kill count risen, with a 250 ms poll behind it.
The file changes for `low`, `high` and `max` too, and for `max` before any kill at that limit (read, `memcg_memory_event` and `try_charge_memcg`), so on an event the agent reads that one file first: a pod near its limit must not spend its watch on them.
Only the pod's file is watched: a container's counts reach it too, so one watch for each pod tells of any of its containers.
A rise starts a burst of closer samples, at most one a minute for a pod, so a job that kills its children in a loop cannot hold the agent in one.
A 50 ms poll of two files for each of 110 pods would be 4,400 reads a second on every node (computed).
Inotify saw each rise 1 to 50 ms before a 50 ms poll, and gave no delete event for any of twelve removed cgroups; a file held open across the removal read `ENODEV` (reported).
So the poll, or a watch on the parent directory (recall), is what notices a removal, and the last values are whatever was read before it.

It races two removals.
The container's cgroup goes first, and the limit is read off the pod's, which stays.
The pod's goes after its final status is set and its volumes are unmounted, with no bound on either side (read, `SyncTerminatedPod`).
On kind the container's went 5 to 20 ms after a kill at its own or its pod's limit and 80 to 220 ms after one above the pod, and the pod's 2 to 2.8 s after its last container was killed (measured, a few kills of each on a busy laptop).
The reader and its tracker alone, with 110 pods on the node, took 3.7 to 5.5% of one CPU at a 250 ms poll and 16 to 23% at 50 ms (measured on kind, before the container's two extra files; no report, no API client).

Only the kubelet's own tree is parsed, at the depths the kubelet makes, so a cgroup a job creates with a pod-like name under its own is not taken for a pod.
Its root is found by name, the shallowest one, and two at the same depth are refused unless the operator names one: kind sets a `cgroupRoot`, which puts it one level down (kind v0.32.0 `config.go`, reported).

## What it writes

Both, on the pod, in the pod's namespace:

- **A pod condition per container**, of type `oom.actions-gateway.com/<container name>`, where the name is the killed container's.
  A condition has no container field and a worker has two that can die, so a single type would let the sidecar's kill overwrite the runner's.
  The API server requires a qualified name (read, `validatePodConditions`), whose name part and a container's name are both at most 63 characters (recall).
  `status: "True"` with the reason above, or `"Unknown"`.
  Written once the pod's status shows that container `OOMKilled`, so a counter alone never marks a pod.
  It says what the **last** kill was: nothing clears it, a container that restarts keeps it, and a second kill with the same reason leaves `lastTransitionTime` where it was.
  So the message carries the time that container ended and its ID, which is what a reader compares.
  Both are the pod's status's, so a report's clock is in neither, and a kill reported again changes nothing.
  One condition is written at most three times a minute, so an agent that changes its mind cannot keep the controller from other nodes' pods.
  For 110 pods with one killed container each that is at most 5.5 writes a second from one agent (computed), and the controller's client has no limit of its own for that to fill (PR 2's).
  The AGC [acts on the first answer](#timing); a later one changes what a person reads.
  A kill the agent was late for, or too far from that end to be its, claims nothing and takes back no answer already written for that container ID.
- **An Event** with the same words, for people, whose reason is `OOM` and the condition's reason, or `OOMProcessKilled` for a kill that ended no container.
  It outlives the pod, and it covers a kill that ended no container, such as a build's child process, where a condition would mislead.
  At most one for each end of a container, by its status, and reason, and one a minute for a pod's kills that ended none, so neither a job that makes its children die in a loop nor a report with many times in it writes a flood.

The kubelet keeps a condition it does not own, through the pod's terminal update too (read, `mergePodStatus`).

## Who holds what

### Two components (D2)

```text
 each worker node                         platform node
┌──────────────────────┐   one report   ┌───────────────────────┐
│ agent                │   object per   │ controller            │
│  not root            │   node         │  pods: get/list/watch │
│  /sys/fs/cgroup: ro  ├───────────────►│  pods/status: patch   │
│  no pod access       │                │  events: create       │
└──────────────────────┘                └───────────────────────┘
```

**The agent** learns pod UIDs and container IDs from cgroup paths, which carry the first (read, `pkg/kubelet/cm`) and the second (recall; the runtime names it).
It writes them, with counter values and times, into one ConfigMap named for its node behind a fixed prefix, in the reporter's own namespace.
It needs `update` there and nothing else.

**The controller** maps UID to pod (a static pod's cgroup has another UID than its mirror pod's (recall), so its kills are dropped), **drops and logs any whose `spec.nodeName` is not the node in the report's name**, which is what holds an agent to its own node's pods, decides the reason, writes the condition and the Event, and copies no string from a report into either: beyond the identifiers it looks up, a report is numbers.
It creates each node's ConfigMap and deletes it when the node has no agent, which it learns from the agents' pods in its own namespace, so it needs no read of Nodes.
A ConfigMap and not a custom resource, because a CRD is a cluster-wide install and the controller checks the shape either way.
It runs where tenants' jobs do not, and the chart has to make that so: it requires node affinity on a label of the `node-restriction.kubernetes.io/` namespace, which a kubelet may not set on its own node (read, `noderestriction/admission.go`).
An ordinary node-pool label is not enough: outside the `kubernetes.io` and `k8s.io` namespaces a label is the kubelet's to set (read, `getForbiddenLabels`), so an escaped job could set one on its node and draw the controller's token there.
Whether GKE can put such a label on the nodes of an autoscaled pool is not read.
Where it cannot, the alternative is a taint on every pool that runs tenants' jobs that the controller does not tolerate, which a kubelet may not change either (read, the same file).

**Why not one DaemonSet that patches pods itself.** Its token would sit on every node that runs tenants' jobs, with `list` on pods, since RBAC cannot narrow a list to one node: a rule matches verb, group, resource and name (read, `RuleAllows`).
Every pod's spec in the cluster would then be one escape away.
An admission policy can hold its writes to its own node and cannot hold its reads at all.
Today the same escape gets the kubelet's credentials, which reach that node's own pods (read, `authorizePod` in `node_authorizer.go`).
So one DaemonSet is a step down for every cluster that runs `privileged-dind` without Kata, and two components are not.

### The least each needs

| | Agent | Controller |
|---|---|---|
| Runs as | Non-root, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp | The same, and no `hostPath` |
| API | `configmaps` update in its namespace | `pods` get/list/watch, `pods/status` patch, `events` create, in the namespaces the chart's values list; `configmaps` get/list/watch/create/delete and `pods` get/list/watch in its own |
| Cluster-wide grant | None | None, unless the operator lists no namespaces and says so |
| Network | The API server only | The API server only |

The option's container is neither: its privilege is [above](#the-kernel-log-as-an-option), and it has no API access.

The service accounts live in a platform namespace.
Kubernetes' `edit` role can mint a token for, and impersonate, any service account in its own namespace (read, `bootstrappolicy/policy.go`), so one in a tenant's namespace would belong to the tenant.

### Admission policies

Three, in the reporter's chart, with `failurePolicy: Fail`:

1. **A report is written only by its own node's agent.** A report's name is a fixed prefix and then the node's name.
   On create and update in the reporter's namespace, any writer but the controller is refused a name with that prefix whose rest is not its token's node claim, and the agent is refused every other name, since its `configmaps` update covers the namespace.
   Other ConfigMaps stay writable by others: the control plane itself keeps `kube-root-ca.crt` in every namespace, which both pods' token volumes mount (read, `rootcacertpublisher` and `serviceaccount/admission.go`).
   It also refuses the agent `immutable`, finalizers and owner references on its report: `immutable` would refuse its successor's writes until the node had had no agent for ten minutes (reasoned).
   The claim is `authentication.kubernetes.io/node-name`, which a projected token carries (read, `ServiceAccountTokenPodNodeInfo`, locked on since 1.32; this chart's floor is 1.35); a token without it is refused.
2. **The controller may change only its own conditions.** `pods/status` is wider than its name: the API server resets the spec, the deletion timestamp and the owner references on such a write, and not labels, annotations or finalizers (read, `podStatusStrategy.PrepareForUpdate`).
   The AGC keeps its recovery marks in annotations, so an unguarded grant could forge "already handled" on any worker.
   The policy requires metadata unchanged but for `managedFields`, every other status field unchanged, every changed condition to be of the reporter's domain, and no more conditions outside it than before.
   A typed object's fields cannot be iterated, so it lists them; a test walks `PodStatus` and `ObjectMeta` and fails on a field the policy does not name, which is how a later release's field is caught.
3. **The controller's Events carry only its own reasons.** It matches both Event API groups.
   The AGC's opt-in `AutoscalerVerdict` mode reads a `NotTriggerScaleUp` or `TriggeredScaleUp` Event by its reason and no other field (read, `classifyAutoscalerEvent`), so `events` create without this could gate or ungate a set's intake in every namespace it covers.
   `edit` can do that today in its own.

Policies 2 and 3 each have one match condition, that `request.userInfo.username` equals the controller's, so a defect in a rule's body reaches no other writer.
That condition itself is evaluated for every `pods/status` write and every Event in the cluster, a kubelet's included, and an error in it denies the write under `Fail`, as a fault in finding a policy's parameters does before it (read, `matchconditions/matcher.go` and the policy's `dispatcher.go`).
So it is that one equality and nothing more, neither policy has a `paramKind`, and neither's binding has a namespace selector, which would leave namespaces unjudged.
Policy 1's binding selects its namespace by `kubernetes.io/metadata.name`, which the API server resets on every write (read, the namespace strategy and defaults), so the label cannot be moved off.
A selector that names the wrong namespace judges nothing, so reading 5 loads the bindings the chart renders.
Its rule reads the node claim for a name with the prefix and no other, since the publisher of `kube-root-ca.crt` has none.
RBAC and policy take the name from one value, with a test.

RBAC alone would let an agent write any node's report, so the chart has no way to install without them.
None is written in the chart.
A draft of the first two exists, and no run of it is cited here; that the language reaches each check within its cost limit, and each refusal, are to be shown in envtest before the rest is built (reading 5).

### Who could forge the answer (R9)

| Who | Event | Condition | Report |
|---|---|---|---|
| A job in a worker pod | No: the AGC builds workers with no service-account token (read, `pod.go`). An unsandboxed `privileged-dind` job can **cause** one on another pod, below | No | No |
| A person or workload with `edit` in the tenant's namespace | **Yes** (read) | No: `edit` and `admin` only read `pods/status` (read) | No |
| A person with `pods/status` from a role the platform added. On GKE `roles/container.developer` is one: it includes `container.pods.updateStatus` ([Google's role reference](https://docs.cloud.google.com/iam/docs/roles-permissions/container), 2026-11-24), and no Kubernetes role shows it | Yes | Yes | No |
| A job that has escaped onto its node: the kubelet's credentials create Events and patch its own pods' status (read, `NodeRules` and `noderestriction`) | Yes, on any pod | Yes, for that node's pods | Yes, its node's |
| A taken-over agent | Through a report, for its node's pods | The same | Its node's |
| A taken-over log reader, with the option on | A sentence, or a `Log` reason's, for its node's pods | A `Log` reason for its node's pods, over `Unknown` or nothing | What the agent takes from its socket |
| A taken-over controller | Yes, with its own reasons | Yes, for any pod in its namespaces, and only its domain | Yes |

So the AGC acts on the condition and never on the Event, repeats it only as [attributed words](#reading-it), and R9 holds where no tenant has `pods/status`, which the operator's page has to name, with the GKE permission.
That GKE role includes all of `container.pods.*` (the same page), so whoever has it can change the pod itself too.
A fourth policy could refuse a condition of this domain from anyone but the controller.
It would run on every kubelet's status write, where a defect stops pod status cluster-wide, so it is not proposed here.
The escape row is no wider than today: such a job already holds the kubelet that writes the status the AGC acts on.

**What a job can cause rather than forge.** It can make its own pod's `oom` rise, with a nested limit, at the moment a neighbour fills the node, or up to a minute before; the answer can then be `Unknown`, which gets it what it gets today.
So can a job with a writable cgroup by clearing its own container's `memory.oom.group`, which the kernel lets a delegated cgroup do (reported, `NS_DELEGATABLE`).
An unsandboxed `privileged-dind` job can go further with no escape, since its daemon has the host's view of the cgroup tree (Q1396): `docker run --cgroup-parent` can name a slice outside its pod (reasoned from that reading, not run).
Under another pod, a nested kill there raises that pod's `oom` (read, `memcg_memory_event`) and not its `oom_group_kill` (read, `mem_cgroup_get_oom_group`), so on its own it would write an Event and no condition (reasoned from both).
Beside a kill made above that pod it can change the condition too: to `Unknown` where the agent sampled the cgroup, and to `ContainerLimit` where it came and went between two samples, or passed for a container's, and the killed container's own file had gone ([the table](#where-it-names-the-wrong-limit)).
That is a false cause on another tenant's pod, and the raise that pod gets today, so it undoes D3's withhold for that pod.
A copy of the whole pod directory beside it silences that pod for as long as either is there, which then gets what it gets today, and the agent logs it.
One made after the kubelet removed the pod's own, as the agent restarts, is [the one that could withhold](#where-it-names-the-wrong-limit).
It can probably also lower the victim's real limits, which is a true kill at a true limit (reasoned).
A second pods' root, or a shallower one, stops an agent that has to find it or misleads it as it starts, so the chart sets `-pods-root`.
So the Event says that a process in the pod's cgroup was killed, and not whose it was, and that pod's `oom` of 0 is no longer conclusive.
Filling another pod's pod-level limit, where that pod's spec has one, makes a true `PodLimit`: no raise and no re-run, where today has both (reasoned, not run).
Under the pods' root it fills the pods' limit from no pod, which is one reason there is [no re-run class here](#the-re-run-class-a-follow-up).
It can drive its own pod over its request until the pods' shared limit runs out, and be the victim; that is real, and its answer is no raise and no re-run.
And it can time an allocation of its own against a kill made above its pod, which can read `ContainerLimit` ([the table](#where-it-names-the-wrong-limit)): a raise and a re-run it would have got today, for its own runner.

## What the AGC does with it

### Reading it

No new grant: conditions arrive on the pod watch it has, where Events do not (read, `charts/actions-gateway/files/agc-tenant-role-rules.yaml`: one pod's Events are listed, by choice with no watch).

The watch reaches the cache and not the reconciler: `workerPodPhaseChangePredicate` drops an update that changes only a condition (read), so the condition needs an edge of its own.
For the record it passes a condition of the reporter's domain that appears on any `Failed` worker, one whose recovery is already claimed included.
A worker update also starts recovery and queues a reconcile (read, `workerPodHandler`), so this edge hands the snapshot store the reason and does neither; PR 7 widens it for a kill that waits, as Q1389's start has.

**It has to keep the answer, not only read it.** A worker is deleted 5 minutes after it ends by default (`DefaultCompletedPodTTL`, read), and GitHub concluded a killed job about 10 minutes after it started (measured, [below](#timing)), so by default the pod and its condition are gone before the job is red.
Nor does the failure record have it: its snapshot is taken as the pod turns `Failed`, before the condition comes, and the predicate drops what follows (read, `workerSnapshots.put` and the predicate).
With the edge, the reason is added to the snapshot already stored, and the failed job's record and its `PlatformFaultOnFailedJob` Event carry it when the job concludes.
That Event calls the kill a platform fault, which reads against a container's own limit named beside it; PR 6 settles the wording.
It stores no second snapshot: that would read the egress proxy's readiness again, after the pod ended (read, `SnapshotScaleSetWorker`), and could report a fault the job never met.
That changes no action, and needs no setting within these limits, each with a test in PR 6, since a condition's reason is free text to whoever can write `pods/status` (read, `validatePodConditions` checks types):

- **It only adds words to a fault that is there.** Any entry in the record's faults raises a Warning Event (read, `reportFailedJob`), so the condition never makes one: it qualifies `container <name> OOMKilled`, which the kubelet's status gave (with exit code 137, for any container but the runner; read, `oomKilled` at `be309274d`), for the container its type names.
- **The words are the AGC's own**, one set for each reason, the counters' five and PR 9's four, and say that a pod condition reported it.
  Any other reason is ignored, and the condition's message is never copied.
- **Not on a container that restarted**, whose condition may be an earlier kill's.
  The runner never restarts, since the AGC builds workers with `restartPolicy: Never` (read, `pod.go`); a restartable sidecar can, and its kill is then recorded as today.
- **The first reason for a container's ID stands**, so a condition that flaps changes nothing after it, with two exceptions: a `Log` reason replaces `Unknown`, and a counters' reason other than `Unknown` replaces either.
  Whoever writes first fixes the words, which is all it fixes.
  The snapshot holds no container ID today, so PR 6 adds it.
- **Under the store's lock, and only while the store still holds the snapshot**, since another goroutine may have taken it and be encoding it.
- **The words are no part of the fault.** A record amended when its worker ends compares faults as text (read, `amendFailedJob`), and a fault that gained words there would be reported twice.

The record's JSON holds the type and reason of every condition its snapshot saw, as written (read, `snapshotOf`), in a log line and the ledger; of them only `DisruptionTarget`'s reason reaches an Event (read, `faultsBy`).
Two losses stay: an AGC restart drops a snapshot whose job has not concluded, and a condition that comes after the completion is not recorded.

For acting on it, a setting, off by default, says that a reporter is installed.
It bounds the wait below and is required for any change of action, so a cluster that happens to carry one changes what a record says and nothing the AGC does.

### Timing

The class decides when the pod is `Failed` and the job's start is recorded.
With the setting on, it would also wait until a condition with one of the counters' five reasons is there or 30 seconds have passed since the pod ended, whichever is first, then do what it does today.

- **The re-run loses nothing.** It already waits for the run to conclude, and GitHub concluded a killed job 10 minutes after it started (measured, three kills, each in the job's first 17 s; `oom_start_wait.go`).
- **The hold exists.** Q1389's start wait keeps such a pod for 15 minutes, and this wait would run beside it, from the pod's own end, so a restart of the AGC does not extend it.
- **It needs widening to two arms.** `oomRetryable` (read) also asks for a memory limit and the sizing profile's annotation, so neither a runner with no limit nor a `Static`, `NodeShare` or `AwaitingSamples` worker is held today; the second is withheld at once as `own_limit`.

The 30 seconds is a guess at several times the agent's and controller's delay, of which 10 are [the hold](#the-rule) of an answer that rests on time.
The agent's part was under 0.1 s on kind, from the first rise it read to its answer (measured); the rest waits for PR 2.

### By reason

| Reason | Today | With the condition |
|---|---|---|
| `ContainerLimit` | Raise and re-run, or a withhold, naming no cause | The same actions, and the text says the limit ran out |
| `PodLimit` | The same; a raise past the pod's limit builds a refused pod ([Q1402](../queue/Q1402.md)) | No raise, no re-run; the Event says the pod's limit |
| `PodsLimit`, `AbovePods` | Raise and re-run, or `own_limit`; the kill can open the set's breaker | No raise, not counted by the breaker, and D3 |
| `Unknown`, a `Log` reason, or none in time | Today's | Today's, with Q1400's text |
| Any, on a runner with no limit | A log line, no Event | An Event that names the reason |

A raise after a kill made above the pod costs twice: it is kept up to 7 days, and under `Throughput` it lifts the limit and not the request, which makes an overcommitted node more so.
That is why the raise goes at once, and the re-run is a separate question.

### D3: the re-run

For `PodsLimit` and `AbovePods`, three options:

| Option | What it leaves |
|---|---|
| **No raise, no re-run** (decided) | A job killed by its node waits for a manual re-run, and its Event says why. Nothing is re-run whose cause could be the tenant's own |
| No raise, re-run at the same limits | Keeps the recovery such a job gets today. It is a new class without R12's evidence, and R10 fails for one shape below |
| As today: raise and re-run | No change to what is re-run; the raise is the wrong one and the text then contradicts it |

Withholding needs none of what follows, since it only does less.

### The re-run class: a follow-up

A re-run on `PodsLimit` is **not part of this design**, and belongs after 2.1 with a grant of its own.
The plan carries what is known so that a later one starts from it.

**R10: whose doing it is.** Being killed above the pod does not mean the tenant did not cause it:

| Shape | Reason | The tenant's doing? |
|---|---|---|
| A neighbour over its request fills the pods' limit; this pod is within its request | `PodsLimit` | No |
| This pod, over its own request, fills it | `PodsLimit` | Yes, or its sizing profile's |
| Another pod of the same tenant, over its request, fills it | `PodsLimit` | Yes |
| The job's own Docker containers, outside its pod's cgroup, fill the node (Q1396, measured on `kindest/node:v1.35.5`) | `AbovePods`: by default they do not count against the pods' limit | Yes |
| The same, started with `--cgroup-parent` under the pods' root | `PodsLimit`, with every pod within its request (reasoned, not run) | Yes |
| A system daemon, or the node sized too small | `AbovePods` | No, and nothing tells it from a job's containers |

So `AbovePods` could never qualify, and `PodsLimit` would need at least:

- **No tenant pod on the node that fails Pod Security `baseline`**, unless its runtime class is on an operator's list of sandboxes.
  A `hostPath` on the cgroup tree or `hostPID` gives the same reach with no privileged container, and a runtime class's name proves nothing (reasoned).
  Tenant namespaces only, since system DaemonSets are privileged on every node.
  So such a class could reach `plain` and `kata-dind` sets and never a `privileged-dind` one.
- **The victim's pod within its memory request**, by construction where every limit equals its request, as `Binpack` sizes the runner, or by `memory.peak` (kernel 5.19 and later, recall), which counts page cache and so errs toward no.
- **No other pod of the same tenant ever over its own**, by `memory.peak` too, since `memory.current` is a sample a job can time around.
  A tenant can have more than one namespace, and nothing here says which are whose.

A `Throughput` runner over its request is the profile's choice and no tenant's or neighbour's; the answer there may be to raise the request, which is the sizing profile's business.

**R11.** A fresh pod can land on the same node, because the scheduler counts requests and not a neighbour's use, so it would take the set-wide breaker.

**R12.** Q1316's run does not supply it: its victims are `privileged-dind`.
That run settles whether the counters hold on GKE, which is what 2.1 needs of it; the class needs an R3 of its own (reading 9).

## What it covers

| Workload | What it would say | Standing |
|---|---|---|
| `baseline` or `restricted` runner, `Burstable` or `Guaranteed` | Any reason | Rule unmeasured on GKE |
| `privileged-dind`, job's own containers | Nothing when one of those is killed at its own limit: it is outside every pod (Q1396). When they fill the node, the runner's kill reads `AbovePods`, or `PodsLimit` under a `--cgroup-parent` | Location measured on kind |
| `privileged-dind`, the sidecar killed | Its own condition; the runner's is untouched | To be read |
| A GPU runner with no memory limit (`BestEffort`) | `PodsLimit` or `AbovePods`; the first thing to say anything about it. Never `ContainerLimit`, since it has none | By the rule |
| A pod-level limit | `PodLimit` | Read on kind |
| Kata | Nothing for a kill in the guest; the virtual machine itself killed by the host reads as any other | Recall; Q1316's R7 |
| gVisor | `ContainerLimit` or above, by pod; which container is expected to be unclear, since the sandbox is one process tree | Recall; not planned |
| Cgroup v1 node | Unsupported. The kubelet refuses to start on one by default (read, `FailCgroupV1`) | |
| GKE Autopilot | Expected not to admit the `hostPath` (recall), so Q1400's text stands | |

## On by default? (D4)

No. It adds a `hostPath` pod on every worker node, a namespace at `privileged` level, and write access to pods' status.
Each is contained above and none is there today, so the operator opts in by installing a chart, and to the AGC acting on it by one setting more.
The gateway's chart neither contains nor depends on it.

## Where it lives (D5)

| | This repository, own module and chart | The gateway's chart, behind a value | Its own repository |
|---|---|---|---|
| Useful without the gateway | Yes: its chart installs alone | No | Yes |
| New release work | One image and one chart on the existing pipeline | One image | The whole pipeline: builds, signing, SBOMs, scans, verification, a release process |
| Privilege in the gateway's chart | None | The namespace and grants above | None |
| Cadence | The gateway's | The gateway's | Its own |
| Cost of changing later | A move, if nothing imports across | A chart migration for operators | None |

The first, with a gate that fails if the module imports a gateway module or the reverse.
The AGC would know one string, the condition's domain.

**What exists already.** `sapcc/kubernetes-oomkill-exporter` reads the kernel log and the containerd socket from a privileged container, and publishes a count per pod (read, its example manifest, 2026-11-24).
It does not say which limit ran out, and it holds more than either component here. cAdvisor's `container_oom_events_total`, which the kubelet serves already, counts against the victim's cgroup and not the limit's, or against `/` for a node-wide kill, whose line names no limit's cgroup (read: `getContainerName` takes `task_memcg` as `ContainerName`, which is the name counted).
So it tells a node-wide kill from the rest, and not `ContainerLimit` from `PodsLimit`. node-problem-detector raises an Event on the Node and not the pod (recall).
No wider search than one query of the web and one of GitHub was made.

## State, through five cases

| State | Restart | Rollout | Rollback | Concurrent writers | A human between |
|---|---|---|---|---|---|
| Agent's last samples, in memory | Lost: kills across the gap are `Unknown` | One node at a time; two agents on one node would write the same object, so the DaemonSet never surges | The older agent overwrites the object whole | One object per node, one writer by policy | Deleting the object loses kills not yet read; the controller recreates it |
| Report object, a bounded list of recent kills | Kept | The shape carries a version; an unknown one is skipped and logged | As rollout | Controller reads only | As above |
| Condition on the pod | Kept, and written again only if it differs | Same type and reasons, or a new domain | Older readers take an unknown reason as `Unknown`, but for PR 7's wait, which it does not end | Controller, kubelet: a strategic merge by condition type, which the kubelet's own write preserves (read). Two controllers write the same value | A deleted pod loses it; the Event stays. `edit` cannot remove it |
| Option: the log reader's place | Kept nowhere: it reads the buffer again, and a line already reported writes the same condition, or nothing where a counters' reason stands | Off and on by value; an agent without the reader ignores its socket | As the option off | One reader per node | Clearing the kernel's log loses lines not yet read |
| AGC's wait | Runs from the pod's end, so it is not extended | An older AGC ignores the condition | The same | The class's claim is unchanged | A manual re-run meets the same claim as today |

## What to measure first

Readings 1 to 4 and 10 were taken on one throwaway kind cluster, in part, on 2026-11-24 (kubelet v1.35.5, containerd 2.3.1, kernel 6.12.76, all nodes one virtual machine).
None is granted on a cloud node.
**kind** answers the mechanism; **dogfood** answers whether GKE's node image agrees.

| # | Reading | Venue | What it decides |
|---|---|---|---|
| 1 | A non-root, unprivileged pod reads every pod's `memory.events`, `.local` and `memory.peak`; one made after it started appears | kind, dogfood; a local container has | D1 |
| 2 | Each row of the rule against the kernel's own line: one container, two limited, one limited beside one unlimited, and a pod-level limit, `Burstable` and `Guaranteed` | kind | The rule |
| 3 | From the kill: first rise seen, by inotify and by poll; container's and pod's directory gone; `Failed` arrived; the status's `finishedAt` against the rise; the agent's CPU at 250 and 50 ms with a full node of pods | kind, dogfood | The watch, the poll, the bound that names the container, and the 30 seconds |
| 4 | Two kills in one sample, a nested one and one at the pods' limit; a nested kill, then a SIGKILL of the same container, with and without a group of its own | kind | `Unknown`, and which container |
| 5 | The three admission policies, with the bindings the chart renders, refuse what they should, in envtest, with the cost limit, and `kube-root-ca.crt` is still written: by its publisher's service account, since envtest runs no controller manager | envtest | D2 |
| 6 | A `pods/status` patch on a `Failed` pod, kept across the kubelet's last write | kind | The condition |
| 7 | Kata: host counters still while the guest kills | dogfood, Q1316's R7 | That row |
| 8 | Q1316's R2, R3, R8 with the kernel's line and every pod's use beside its request | dogfood | The raise, on GKE |
| 9 | R3 on a set whose node has no tenant pod past `baseline`, and `--cgroup-parent` under the pods' root from one that has | dogfood, after 2.1 | The re-run class alone |
| 10 | A container as the option's reads a kill's line by `syslog(2)`; refused without `SYSLOG`, at uid 65534 with it, and under `hostUsers: false`; no token in it; each line the parser must refuse, made by a job that names its cgroups; how long a line lasts; `dmesg_restrict` and `kptr_restrict` | kind (PR 9's lane in CI is one), then dogfood | The option |

What the kind run read, beside what the sections above cite:

- **1.** A pod at uid 65534 with no capability read every pod's files through a read-only `hostPath`, those of pods made after it among them, and found the pods' root two levels down.
- **2.** Every row but the node's own memory: thirteen kills of whole containers, the rule and the kernel's line agreeing on twelve.
  The other came 1.4 s after a nested kill and read `Unknown`, as it should.
  At the pods' limit there were 2 or 3 `oom` for each of three kills.
  PR 1's tests replay the readings; `cgroup.stat` was not among them.
- **3.** The watch and the removals.
  The status's `finishedAt` has whole seconds, so it was not compared with the rise, and when `Failed` arrived was not timed.
- **4.** A nested kill alone raised the pod's `oom` and no whole kill.
  A nested kill 2.05 s before one at the pods' limit read `PodsLimit`.
  A container whose process ended by itself on SIGTERM after a nested kill was marked `OOMKilled` with exit **143**: with no whole kill it would get no condition.
  Not read: a nested group of its own, a SIGKILL of the container after a nested kill, and two containers killed in one sample.
- **10.** uid 0 with `SYSLOG`, `RuntimeDefault` and no token read the buffer by `syslog(2)`, with the victim's line at `<6>` and the line that names its group.
  `EPERM` without `SYSLOG`, at uid 65534 with it, and under `hostUsers: false`, where the capability was in the effective set, so that refusal is the kernel's (read: the seccomp profile allows the call with it).
  The other two were not told apart.
  `dmesg_restrict` was 1 and `kptr_restrict` 0: with `dmesg_restrict` at 0 the kernel lets an unprivileged reader do this (read, `check_syslog_permissions`), so the two refusals that had `SYSLOG` in the spec cannot be counted on there; without it the runtime's default seccomp profile still refuses (reported, containerd v2.2.0).
  Not read: the lines the parser must refuse.

Not run: two containers killed at once, several threads or pods at the pods' limit together, and a cluster whose containers have no `memory.oom.group`.

Q1316's plan takes readings 1, 3, 7 and 8 in its runs as its branch had them on 2026-11-24, but for reading 3's CPU, which needs the agent built; its non-root reader is a container of the job's own Docker for R2 and R8, and a pod in R3, R3t, R4 and on the Kata node.
It prints `oom_group_kill` and each container's own rise beside the pod's, so R8, with two containers in the pod, reads the join as well as the limit.
It does not read whether a non-root pod could open `/dev/kmsg` or the journal through a `hostPath`, which would be another way to the log.
Nor reading 10, since the job's own privileged container opens `/dev/kmsg`; on GKE that is one more pod on a node the run already has, if the run's plan takes it.
Nor does it run a one-container pod or a pod-level limit: either answer to the first is `ContainerLimit` by the rule, and what decides both is the kubelet's cap and the kernel, which a local node reads.

## Build size

Sizes are estimates.
PRs 1 to 6 are [Q1403](../queue/Q1403.md), which gates 2.1; the three that do not gate it have rows of their own, so that its label does not hold the tag for them.

| # | PR | Size | Needed for |
|---|---|---|---|
| 1 | The counter reader and the rule, tested on figures a local container gave, and on a kubelet's files once there are any; finding the pods' root | M | The tool |
| 2 | Agent, report ConfigMap, controller; condition and Event | M | The tool |
| 3 | Chart: RBAC, the admission policies and their envtest, the node affinity, NetworkPolicy; the no-import gate. From PR 2's review: policy 1 refuses the agent `immutable`, finalizers and owner references, the chart sets `-pods-root`, and a test holds a condition's writes and the Events to their limits against a real API server | M | The tool |
| 4 | Image and publish: a `Dockerfile` stage, `docker-bake.hcl`, the `publish.yml` matrix (seven images become eight) and a third chart, SBOM, Trivy, `verify-release.sh` and the docs that count seven | M | The tool, outside a source build |
| 5 | kind e2e: one kill per reason, the condition and the Event; the operator's page | M | The tool, trusted |
| 6 | AGC, the record: the edge, and the reason in the failed job's record and Event, with a test for each of [its limits](#reading-it) | S | The answer outliving the worker |
| 7 | ([Q1406](../queue/Q1406.md)) AGC, the action: the setting through the GMC, the wait, text by reason, no raise and no re-run above the container; its e2e and docs | M | D3 |
| 8 | ([Q1408](../queue/Q1408.md)) The re-run class | M, and a dogfood run | After 2.1 |
| 9 | ([Q1407](../queue/Q1407.md)) The kernel-log option: the second container and its chart value, the parser and its fixtures, the two-witness table in the controller, its words in PR 6's record, an e2e lane with it on (reading 10) and its part of the operator's page | L | D1's option |

**Order.** 1, then 2 and 4 together, 3, 5; 6 any time after 2 fixes the condition's type, and 7 after 6.
9 after 5 and 6.
PRs 1 to 5 have no gateway code and can run beside the 2.1 stream; 6 needs the edge Q1389 landed in the same predicate, and 7 the text Q1400 did.
PR 1 starts from files a local container gives.
A kubelet's are the kind run's, which PR 1 carries as its test data.
For scale of PR 4: the newest image, `cache-runner`, is named in 16 files outside `docs/plan`, `docs/queue`, past release notes and the dogfood runner (`git grep -l`, 2026-11-24), some of them about that image's own use.

### The smallest set for 2.1

What it has to do is let somebody tell one root cause of an out-of-memory kill from another.

| Set | PRs | What it lands | What it leaves |
|---|---|---|---|
| Tool alone | 1 to 5 | The condition and the Event on any pod | For the gateway's own workers little more than the Event: the pod is gone before the job is red |
| **Tool and record** (the gate) | 1 to 6, and reading 8 | The reason in the failed job's record and Event, when the job concludes | The AGC still raises on a kill made above the pod, beside a record that says so |
| Tool, record and action | 1 to 7, and reading 8 | No raise and no re-run above the container, and text by reason | The re-run class |
| Event only | 1 to 5, each smaller | The Event, with no `pods/status` grant, no policy 2 and no condition | An answer that expires with Events, after an hour by default, that `edit` can forge, and that the AGC could neither keep nor act on |

Beside today, this set makes nothing worse in action where it is installed, and nothing at all where it is not.
Reading 8 is Q1316's dogfood run, which is the only test of the rule on GKE: without it the tool says on its page that GKE has not been read.
PR 4 cannot follow a release that claims the tool, since an unpublished image is a source build.
PR 5 could, and then the rule ships with nothing that kills a container on purpose behind it.
D2's single DaemonSet would save the report ConfigMap, one policy and about a third of 2 and 3.

**Where PR 9 sits**, for any of these sets:

| Choice | What it leaves |
|---|---|
| **Rides 2.1 where it is ready, and does not gate it** (decided) | 2.1 may ship with the counters alone. Reading 8 says how often that matters, and nothing the AGC does depends on PR 9 |
| Gates 2.1 | Seven PRs, and reading 10 on kind at least; unread on GKE unless Q1316's run takes it |
| After 2.1 | The gaps in the table above stay `Unknown` in 2.1 |
