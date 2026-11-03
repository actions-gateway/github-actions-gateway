# AGC per-scale-set memory probe (Q722)

`TestScaleSetPerListenerMemory` (`scaleset_mem_test.go`) isolates the Actions Gateway Controller's (AGC) own memory cost of one live scale-set session.
It needs no cluster and no real credentials.

```bash
make scaleset-mem-profile   # 200 parked scale-set sessions; prints bytes/set
```

Knobs, read from the environment:

| Variable | Meaning | Default |
|---|---|---|
| `SCALESET_MEM_SETS` | scale sets (= `Listener`s = sessions) | `200` |
| `SCALESET_MEM_SETTLE` | settle wait after sessions park | `1s` |

## What it measures

One scale-set `Listener` holds one *scale set's* acquisition session and multiplexes every job assigned to that set through it, so the resident cost scales with the RunnerSet count and not with the number of concurrent jobs.

A representative run holds 200 scale sets at **~7.7 KiB/set** (≈ 4.8 KiB goroutine stack + ≈ 2.9 KiB heap), with the `scaleset.Client` and `Listener` structs a further ~1.2 KiB built before any session opens.

The probe runs against `scalesetMemTransport`, an in-process `http.RoundTripper` with no server, socket, or per-session server-side state.
`scalesettest` serves the real protocol model and is the right venue for behaviour, but its `httptest.Server` holds a parked goroutine and its read/write buffers per session in this same process, which would inflate the figure.

The probe asserts every one of its polls is actually parked before it samples: a goroutine count cannot tell a resting long poll from one spinning in a backoff retry, and the figure divides by the set count, so a short parked count would compute it over the wrong denominator.

> **Boundary.** This isolates the AGC session *structures*.
> It excludes the per-connection HTTP transport buffers an active long-poll holds in production, a real but bounded additional cost.

## Files

| File | Role |
|---|---|
| `doc.go` | package godoc (untagged) |
| `memory.go` | heap+stack sampling, the in-memory response builder, and the env-knob readers |
| `scaleset_mem_transport.go` | stub-free in-process `RoundTripper` for the scale-set protocol's bootstrap and resting long poll |
| `scaleset_mem_test.go` | `TestScaleSetPerListenerMemory`; reads `SCALESET_MEM_*` env knobs |
