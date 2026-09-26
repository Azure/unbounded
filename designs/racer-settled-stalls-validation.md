# Zero-budget retained-copy validation

Base: `5094910b69c49d07d595286c9ce87a394862f881`.
Worktree/branch: `tmp/racer-fix-settled-stalls`, `racer-fix-settled-stalls`.
Affected artifact: **racer-dataplane**. Reviewed implementation is included in this authorized fix commit.

## Contract and single defect

Zero-credit Acquire may look up existing copies but must not acquire from origin
(`designs/racer-peer-security.md:95-102`). The base Fill checks memory, then joins
an acquisition flight before checking pending/disk copies. Flight admission
rejects zero attempts (`cmd/racer-dataplane/src/read/flight.rs:606-623`). Thus a
valid disk or pending copy can be inaccessible through Acquire even when
CopyOnly successfully returns it. Deterministic tests establish this defect;
usable-copy availability during the live failures remains unproven.

The only production change is the zero-attempt memory-miss branch in
`cmd/racer-dataplane/src/read/fill.rs:165-200`: use existing CopyOnly lookup, clamp
its scope to the original budget deadline, reserve plaintext, and perform normal
AEAD decryption. Missing copies return Unavailable. Identity validation precedes
lookup (`:157-159`). Neither candidate resolution nor origin acquisition is
invoked by this branch. Acquisition election still rejects zero attempts
(`cmd/racer-dataplane/src/read/flight.rs:450-459`).

CopyOnly may join existing work without electing or reviving acquisition, under
the existing waiter limit and request scope
(`cmd/racer-dataplane/src/read/flight.rs:690-743`). Disk reads retain existing
staging, mapping validation, and completion ownership
(`cmd/racer-dataplane/src/store/reader.rs:70-163`); decryption uses owned crypto
inputs/completions (`cmd/racer-dataplane/src/security/aead.rs:89-117`). Plaintext
admission performs one idle eviction and one retry. Live owners still filling
the quota cause Overloaded rather than a new wait loop retaining ciphertext.

## Accounting

No production budget, quota, timeout, route ceiling, or wire contract changed.
Destination inherits attempts verbatim and debits only the final link
(`cmd/racer-dataplane/src/read/serve.rs:51-70`); relay preserves attempts while
debiting a link (`cmd/racer-dataplane/src/peer/relay.rs:102-114`). CandidatePolicy
debits an outgoing attempt and partitions a separate remote allowance
(`cmd/racer-dataplane/src/read/candidates.rs:294-307`). Eight attempts can allocate
remote allowances 3, 2, 0 across three failed candidates. No double attempt debit
was identified in these paths. No refund or new credit is introduced.

Existing assertions cover conserved candidate allocation
(`cmd/racer-dataplane/src/read/candidates.rs:431-478`) and inherited zero-budget
attempt rejection (`cmd/racer-dataplane/src/read/serve.rs:429-465`). They prohibit
unfunded acquisition, not retained-copy service.

## Exact local old-fail/new-pass

`cmd/racer-dataplane/tests/production_dataplane.rs:1035-1109` seeds a full **16 MiB**
page through actual origin HTTP, flushes real O_DIRECT persistence, disables
origin, evicts memory, and verifies no pending writes or retained plaintext/
flight shortcut. CopyOnly returns the exact version, and real AEAD decryption
matches every seeded byte. Normal Fill then receives that same PageId with zero
attempts and links; it must return matching plaintext without additional origin
calls or changed credits. CopyOnly has no acquisition-budget argument.

This test uses **128 MiB plaintext, 128 MiB ciphertext, 64 MiB dirty, two neighbor
connections, and eight relay slots** (`:1037-1048`). These match the relevant
observed worker limits; other Rig settings remain fixture settings. It is a
single-node production-component regression, not a full fleet configuration.

`cmd/racer-dataplane/src/read/fill_tests.rs:276-437` tests a real pending writer
copy with a full-page success case, plus miss, wrong version, wrong context,
pre-canceled scope, earlier budget deadline, corrupted ciphertext, and exhausted
plaintext quota. Pending storage receives a separately owned ciphertext copy so
memory can be evicted without the writer pinning its plaintext bundle. The test
asserts no disk entry, actual pending ownership, no additional origin call,
zero unchanged credits, and final zero byte/flight/waiter charges. Its NoPeer
adapter panics on any peer acquisition (`:33-41`). Failure cases use small pages.

For the **final versions of both tests**, only the 37-line production hunk was
temporarily removed with apply_patch. `git diff --exit-code` against the exact
base confirmed Fill was identical. Tests were retained throughout:

| Case | Exact base Fill | Restored fix |
| --- | --- | --- |
| Full-page disk | Unavailable at expected success assertion | pass |
| Pending-copy/boundaries | Unavailable in first pending success case | all cases pass |

Logs: ignored `tmp/final-zero-{old,new}-{disk,pending}.log`. The exact production
hunk was restored immediately after the two old-code runs. No baseline switch
remains. Older exploratory logs remain separate and are not the final comparison.

## Verification

Each cargo command below ran under `timeout 280s`, subprocess timeout 285s, and
an outer 290-second wrapper. Completed logs were inspected before this report.

| Command after `cargo` | Result | Ignored log |
| --- | --- | --- |
| `test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib -- --test-threads=1` | **571 passed, 14 ignored** | `tmp/final-zero-lib.log` |
| `test --manifest-path cmd/racer-dataplane/Cargo.toml --release --test production_dataplane -- --test-threads=1` | **11 passed** | `tmp/final-zero-integration.log` |
| `build --manifest-path cmd/racer-dataplane/Cargo.toml --release` | **pass** | `tmp/final-zero-build.log` |

The 14 ignored library tests were not executed in that library run: seven SDK
variants, five full-image TCP/Fill cases, one replay-load case, and one codec
benchmark. Separately, **all seven existing SDK variants actually ran and passed**
with `--lib sdk_ -- --ignored --test-threads=1 --nocapture`, each warm and cold,
checking manifest/config and all eight layer lengths/digests (542950400 layer
bytes per image). Evidence: `tmp/zero-copy-check-sdk.log`. The other seven ignored
tests were not run in this verification. Earlier scoped read suite: **83 passed**
(`tmp/zero-copy-check-read.log`).

Crate `cargo fmt --manifest-path cmd/racer-dataplane/Cargo.toml -- --check` and
`git diff --check`: **pass**. Scoped repository formatting also passed:

```sh
timeout 280s env GOTOOLCHAIN=go1.26.6 make fmt GO_PACKAGE_DIRS=cmd/racer-dataplane/tests/conformance/production_stream_fixture_test.go.txt GO_PACKAGE_PATTERNS=./pkg/racersdk
```

Installed golangci-lint 2.11.4 was built with Go1.26.5; Go1.26.6 was confirmed.
Make ran gofumpt and the configured linter with `--fix`, reporting **0 issues**
(`tmp/final-zero-make-fmt.log`). No Go files changed. No full repository lint or
all-target Clippy result is claimed.

## SDK and live limitations

There is **no genuine zero-credit last-candidate SDK-chain regression**. Existing
SDK tests seed only the first-ranked candidate and discard pending writes
(`cmd/racer-dataplane/src/peer/production_stream_tests.rs:348-374`); their polling
loop does not flush writers (`:328-346`). The forced-relay adapter controls the
first hop, not selective earlier-candidate failure (`:56-97`). Reproducing this
exact chain requires third-ranked persistence and deterministic first-two
failures under unchanged candidate accounting. Existing SDK results establish
compatibility; the disk/pending tests establish this fix's local mechanism.

Earlier read-only live probes established request-tagged zero-budget rejection,
but not useful pending/disk bytes at that instant. Overload resource attribution
and dependency-cycle hypotheses remain unresolved. No claim is made that this
fix explains every live EOF, restores fleet health, or has been deployed.

Detailed investigation and raw artifact index: ignored
`tmp/settled-stalls-investigation.md`, `tmp/settled-stalls-final.md`, and
`tmp/settled-local-contract.md`. Final-candidate branch matches are in
`tmp/phase2-zero-matched.json`; copy-availability observations are in
`tmp/copy1-summary.json`. All six earlier tracer cleanup checks returned exit 1
with no bpftrace matches; cleanup artifacts are indexed in the investigation.
No live probes, cluster mutations, or host changes occurred during local work.
This authorized commit includes the fix, regressions, and validation report.
No push, image build, or deployment has occurred.
