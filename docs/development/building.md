# Agent reference: Building

All binaries are built into `.build/` at the repo root (gitignored). Use the root `Makefile`:

```bash
make build        # build all binaries → .build/agc, .build/gmc, .build/probe, .build/proxy
make build-agc    # build only the AGC controller
make build-gmc    # build only the GMC controller
make build-probe  # build only the probe
make build-proxy  # build only the egress proxy
```

`cmd/worker` is a workspace module but has no dedicated root-level build target — it is built into its container image only.

Individual module Makefiles (e.g. `cmd/agc/Makefile`) also output to `.build/` via a relative path (`../../.build/`), so both `make` invocations land in the same place.
