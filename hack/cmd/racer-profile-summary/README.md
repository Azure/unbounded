# Raw CPU pprof summary

This is an independent, offline profiling analysis tool, not a collector or a
runtime dependency of Racer. It bypasses Parca v0.28.0's TOP aggregation by empty
Function.Name for profiles with populated SystemName fields and
zero addresses: names are filled before analysis, and addresses are never used
as aggregation keys. This was verified against raw PPROF exports; Parca TOP's
name-only grouping is in `pkg/query/top.go:108-131` at tag v0.28.0. Zero addresses
are not the direct cause of that bug. This tool does not change the Parca server
or claim to fix every standard pprof grouping rule.

Run from this directory (a standalone module pinned to the pprof version already
in the repository's go.sum, but not declared in the root go.mod). This avoids
changing application dependencies just to analyze captured profiles:

```sh
go run . -input capture.pprof -normalized normalized.pprof > summary.json
go run . < capture.pprof > summary.json
go tool pprof -top -sample_index=cpu normalized.pprof
```

`-sample-type` defaults to `cpu`; the selected type must have unit `nanoseconds`.
Missing or ambiguous matches, malformed profiles, negative values, and total
overflow fail rather than silently selecting counts or another sample column.
`-normalized` requires a new destination path and fails explicitly if it already
exists, including when it is the raw input path, a symlink, or a hard link to it.
No existing output is overwritten. Keep shell-redirection output paths distinct
from the input too: the shell can truncate a file before this tool runs.

JSON includes the selected column, total sampled CPU nanoseconds, duration,
sample-record count, unattributed nanoseconds for empty stacks, flat and cumulative
rankings, and totals for exact sample-label sets (including numeric labels/units).
Labels such as process name or PID are preserved if the collector supplied them;
the tool does not infer process identity from symbols.

Empty Function.Name is filled from SystemName before aggregation and export.
Aggregation keys are function ID plus mapping ID, never instruction address.
Unsymbolized frames use location ID plus mapping ID, so zero-address symbols do
not collapse. IDs are profile-local. Mapping file/module and build ID are included.
Separate function records remain separate even if their displayed names match.
Flat CPU belongs to the innermost frame; cumulative CPU counts each identity only
once per sample stack, including recursive stacks and inline frames. Both lists
are descending by their respective totals with deterministic identity tie breaks.

These are sampled CPU totals, not wall time, and not an automatic kernel-only
classification. Kernel attribution requires the captured symbols/mappings and
collector context. The tool intentionally does not guess from function names.
The normalized export only fills missing names; it preserves all sample columns,
labels, mappings, and addresses. Standard pprof may apply its own merging rules;
the JSON identity-preserving report is authoritative for this tool.

## Testing and project lint

Root `go test ./...` and `make fmt` skip this nested module. Run these commands
from this directory to test and apply the same formatter/linter configuration:

```sh
timeout --signal=TERM --kill-after=10s 300s gofumpt -w main.go main_test.go
timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.6 golangci-lint run -c ../../../.golangci.yaml --timeout=3m --fix ./...
timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.6 golangci-lint run -c ../../../.golangci.yaml --timeout=3m ./...
timeout --signal=TERM --kill-after=10s 300s go test -mod=readonly -timeout=5m ./...
```

The explicit lint toolchain is compatible with the installed Go 1.26-built
golangci-lint; otherwise a newer default Go toolchain can produce export data
that this linter cannot read. Use an appropriately rebuilt linter when upgrading.
