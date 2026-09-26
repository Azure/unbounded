# Live restart classification and remaining EOF investigation

Base: `03c240def2eb357d19e36639137d1a1cedc41f8b`.
Worktree/branch: `tmp/racer-fix-live-restarts`, `racer-fix-live-restarts`.
## Final result and verification (authoritative)

Approved incoming-peer idle reclamation is implemented and reviewed. Identical
real SDK full-image graph fails with reclamation disabled and passes warm/cold
with it enabled. The baseline failure stops pinned layer continuations at 16 MiB.
Normal checkout remains fail-fast; only completed accepted-peer keepalives may
yield capacity to outbound peer work. Pending readiness owns FD and quota through
cancellation fencing. Bytes available before the reclamation peek protect the
next header; later arrival may race with pressure closure. Active and partial
exchanges are preserved. No quota increase or automatic request retry was added.

Final checks:

- Release library suite, serial: **567 passed, 12 ignored**, 30.07 seconds.
- Production integration suite: **10 passed**, 16.39 seconds.
- Reactor suite, serial: **25 passed**, including ownership/cancellation fences.
- Focused HTTP suite: **28 passed** before the additional global-fence test;
  that new cancel/drop/close/deadline/release test passed separately and is included
  in the final 567-test library run. Peer-server suite: **6 passed**.
- All **five SDK variants** passed warm and cold manifest/config/eight-layer
  exact-size/SHA-256 checks. The first 20-second serial batch completed three
  variants before timeout; only its two unfinished variants were rerun with
  `timeout 18s`, both passing in 12.32 seconds. No full-batch pass is claimed.
- Crate rustfmt check and Go fixture gofumpt check: **passed**.
- Release build: **passed**. Final all-target release Clippy in warning mode:
  **completed successfully, zero diagnostics on added lines**, checked against
  `git diff --unified=0`. Existing strict `-D warnings` failures remain, including
  58 library findings from the earlier run; unrelated lint was not changed.
- Scoped `make fmt GO_PACKAGE_DIRS=cmd/racer-dataplane/tests/conformance/production_stream_fixture_test.go.txt GO_PACKAGE_PATTERNS=./pkg/racersdk`
  ran gofumpt, then the installed Go-1.26-built golangci-lint panicked on Go 1.27
  source. This toolchain limitation is recorded, not presented as a successful
  make-fmt run. No unrelated files were changed.

Final logs are under ignored `tmp/`: `incoming-idle-lib-serial.log`,
`incoming-idle-production-tests.log`, `incoming-idle-reactor-tests.log`,
`incoming-idle-all-sdk.log`, `incoming-idle-remaining-sdk.log`,
`incoming-idle-final-clippy.{jsonl,log}`, and `incoming-idle-final-build.log`.
The causal comparison is `incoming-idle-sdk-old-fail.log` versus
`incoming-idle-sdk-restored-pass.log` and `incoming-idle-sdk-final-pass.log`.

Live binding-matched overload and outbound connection exhaustion are observed;
live application-idleness remains an inference from socket state and code.
The local production SDK graph establishes the selected idle-starvation mechanism
and its fix, not the cause of every fleet EOF. No production tracing code,
baseline-disable toggle, deployment, AKS mutation, or push is included. All live
bpftrace processes were confirmed absent on both traced nodes. Raw traces and
scripts remain gitignored, with no page payloads or authorization/key material.

The user authorized exactly one fix commit based on the SHA above. The following
sections are **historical phase notes**, retained as an evidence trail. Earlier
statements such as blocked, uncommitted, no implementation, no SDK pass, or pending
approval describe their respective phase and are superseded by this final summary.

## Historical phase notes

## Bounds and workload

September 26, 2026, AKS context `joolshev-scale-test`, namespace
`unbounded-system`. Every live kubectl command explicitly used
`--context=joolshev-scale-test --request-timeout=20s` wrapped in `timeout 30s`.
Shell tool timeouts were at most 41 seconds across the recorded phases. No deployment, configuration,
quota, sysctl, or durable cluster state changes were made.

Target image is `loadgen.invalid/benchmark/image` at
`sha256:2aa1e8745512f4f76499b8b9a5543642630d3f1120668d8636278537d9aa53a6`:
eight nominal 64 MiB layers, 20% jitter, seed `benchmark-v1`, pull concurrency
one, layer concurrency four, verification enabled. These are the supplied
workload parameters, not a newly completed full-image experiment.

## Fresh startup evidence

At **15:32:57.897852 UTC**, a fresh fleet GET returned:

- 1500 dataplanes, 1500 Ready, 1164 restarts on 916 containers.
- All 916 retained last terminations were `Error`, exit code 1; none was
  `OOMKilled`.
- 913 failed-container lifetimes were 30 seconds, three were 31 seconds.
- Last termination timestamps ranged from 15:24:34 through 15:25:17 UTC.

Fresh sampled container status and `logs --previous --timestamps --tail=100`:

| Dataplane suffix | Restarts | Last failed lifetime UTC | Previous log |
| --- | --- | --- | --- |
| `6dbdv` | 2 | 15:24:35-15:25:05 | `2026-09-26T15:25:05.997666911Z racer-dataplane: DeadlineExceeded` |
| `ptfrx` | 1 | 15:24:09-15:24:39 | `2026-09-26T15:24:39.295079565Z racer-dataplane: DeadlineExceeded` |
| `j8w7b` | 1 | 15:24:08-15:24:38 | `2026-09-26T15:24:38.741882706Z racer-dataplane: DeadlineExceeded` |

Gantry `226zq`, `cn4zm`, and `nwhxc` had zero restarts. The sampled dataplane
image was the expected base revision, without a request-timeout override or
startup/liveness probes. Kubernetes lastState cannot classify every earlier
restart of a repeatedly restarted container.

Code gives bootstrap a single request-timeout scope
(`cmd/racer-dataplane/src/app.rs:208-209`), defaulting to 30 seconds
(`cmd/racer-dataplane/src/config.rs:167`). Transient retries retain that scope
(`src/app.rs:310-323`), whose expiration exits the bootstrap driver
(`src/app.rs:343-347`). The executable prints the error and returns failure
(`src/main.rs:8-10`). Paths abbreviated with `src/` in this paragraph are
relative to `cmd/racer-dataplane`.

`cmd/racer-dataplane/src/app_integration_tests.rs:689-703` explicitly asserts
deadline failure during bootstrap overload/backoff. This is a strong startup
deadline lead, not identification of the live operation that consumed the
budget. Bootstrap retry semantics are outside this investigation's fix scope.

## Settled EOF and fresh current logs

The supplied sibling evidence is under
`tmp/racer-v2-cluster-validation/tmp/`:
`racer-03c240-c1-settled.{json,md}`, `racer-03c240-settled-fleet.json`, and
`racer-03c240-settled-summary.md`.

The summary records a 15:30:07-15:30:25 probe, 12 premature HTTP 200 EOFs,
two HTTP 503s, ten passing layers, and zero complete images of three
(`racer-03c240-settled-summary.md:4,18`). Sample identities and restart counts
were stable throughout (`:33`). Failures therefore did not coincide with the
observed startup exits. Concrete node 00004b failures include stopping at
16,777,216 of 59,757,056 bytes and at 67,108,864 of 68,935,168 bytes (`:26`).

Fresh current-log requests in the continuation phase:

- Dataplanes `6dbdv` and `j8w7b`: `--since=15m`, bounded tails 70 and 40,
  respectively; both successful commands returned no log lines.
- Dataplane `ptfrx`: current `--tail=30`, no time filter; no log lines.
- Gantry `226zq` and `nwhxc`: `--since=15m`, tails 70 and 40; no log lines.
- Gantry `cn4zm`: current `--tail=30`; only four startup messages, dated
  01:37:09 and 01:40:34, including version `4b898044...` and startup gate release.
- Loadgen `zhs6j`: `--since=5m --tail=12`; no log lines.

No new full-image result or new per-request internal error is claimed.

## Traced failure propagation

Implementation was inspected before assertions and the prior validation report.

1. SDK `Value.Read` finishes bootstrap, then opens one pinned range for all
   remaining pages (`pkg/racersdk/value.go:117-142`). A short body becomes a
   terminal read error (`:157-175`).
2. Gantry aborts the already-started HTTP response on SDK stream failure
   (`internal/gantry/mirror/racer.go:135-147`). That handler emits no error log.
   Its existing continuation-failure test explicitly requires HTTP 200 followed
   by `io.ErrUnexpectedEOF` and partial correct bytes
   (`internal/gantry/mirror/racer_test.go:494-541`). Thus an EOF at the first
   16 MiB boundary is consistent with continuation failure, but does not
   distinguish admission rejection, acquisition failure, or transport failure.
3. Dataplane sends success headers before subsequent page acquisition and
   delivery (`cmd/racer-dataplane/src/client/response.rs:109-122`). Either can
   return a late error. Client listener completion discards the operation result
   (`src/client/listener.rs:179-182`), explaining why empty logs do not exonerate
   those paths. These abbreviated paths are relative to `cmd/racer-dataplane`.

This establishes the EOF propagation mechanism, not its live underlying cause.

## Raw-FD parallel failure lead

The production reactor wraps an accepted completion in an `OwnedFd` and keeps
it in the entry until fenced
(`cmd/racer-dataplane/src/runtime/reactor.rs:874-901`). Cancellation drops that
owned result in `Entry::finish` (`:244-257`).

The test `accepted_descriptor_is_retained_until_cancel_fence` transfers its
socket ownership using `into_raw_fd` (`:1589-1593`), drives both completions,
then calls `fcntl` on the closed numeric descriptor (`:1622-1633`). Another
thread can open a new descriptor with that number between close and assertion.
FD-number reuse remains an explanation, not a reproduced diagnosis. Nothing in
that failure alone proves a production double-close or premature close.

The prior report agrees that reuse is only an inference and records serial
success (`designs/racer-settled-pulls-validation.md:112-116`). Its accepted-idle
reclamation cause and regression are specific to the earlier failure
(`:85-104`); it explicitly leaves other EOF causes unresolved (`:126-133`).
No contract contradiction requiring a change was established.

## Initial next-step assessment

Current logs are insufficient to select a live underlying error. Next collect
a bounded, synchronized per-request observation of the SDK pinned continuation
and dataplane terminal error on one sampled node, retaining only status/error,
page number, byte count, and timestamps. Do not collect authorization headers or
page payloads. A continuation rejection before headers and a failure after
headers must be distinguished before selecting a local fault scenario.

Then reproduce that exact condition in the existing production SDK fixture
(`cmd/racer-dataplane/src/peer/production_stream_tests.rs:19-33`), preserving
four concurrent layers and all eight verified layers. The fixture currently
covers one worker per peer, not Application startup or Gantry (prior report
`:126-127`). Its Go fixture build has an internal 280-second timeout
(`production_stream_tests.rs:46-58`), so it was not started within this short
phase. At that point no local old-fail regression or test run was claimed. Do not choose a
fix, increase quotas, or alter bootstrap semantics on these observations alone.

## Subsequent bounded live probe

Reviewed the parent helper implementation before reuse:
`tmp/racer-v2-cluster-validation/tmp/validate-ef461-live-once.py:18-60,63-69,96-119`.
Its remote program performs HTTP GETs and hashes responses in memory. No remote
files are created or changed. Local `tmp/probe-one-layer.py` reuses only helper
definitions, prepends `timeout 30s` to kubectl, and imposes a 35-second alarm.
It was run with `timeout 36s python3 tmp/probe-one-layer.py`.

Raw evidence: this worktree's ignored `tmp/live-one-layer.json`.
Window: **15:36:48.677805-15:37:03.242888 UTC** on node `vmss0000a8`.
Gantry `cn4zm`, IP `10.243.134.60`, layer digest
`sha256:515e6f5a38f01eda728d0b6497ff117551ea816b14b684ffae35d2134d710c9e`:

- HTTP 200, declared length 68,935,168, matching digest header and
  `Gantry-Mirrored: 1`.
- Received **16,777,216 bytes**, then the client's read timed out after
  **12.0582 seconds**. This observation is a client timeout, not a newly observed
  server EOF. It does not identify what the continuation was waiting for.
- Background pull error counter 32784 -> 32797; success stayed 13 -> 13.
  These counters are not a per-request correlation or an internal cause.

## Live process and effective-budget comparison

A bounded read-only Python invocation through existing net-node `kwgql` read
`/proc/*/comm`, status, thread names, and cgroup. It identified PID **333275**.
The cgroup container ID matched a separate fresh pod GET:

- Pod `racer-dataplane-ptfrx`, UID `aa7d9090-f24d-48c6-ad36-8f3f61295304`.
- Container `cb0285e8381f5a743dfc3decd38cbdf68f62de12a493193b2d56ea8331b838e6`.
- Restart count 1. No environment overrides for byte limits, neighbor slots,
  request timeout, or range-window size.
- Threads: caller `racer-dataplane`, `racer-crypto-0`, `racer-crypto-1`,
  `racer-io-1`, and two `iou-wrk-333275` threads. This confirms two worker pairs;
  the main thread's `Cpus_allowed_list: 0` is its pinned affinity, not the
  container's total CPU allowance. RSS was 411880 kB.

Effective budgets are inferred from these process/config observations and code,
not read directly from live admission counters. Defaults are 256 MiB plaintext,
256 MiB ciphertext, 128 MiB dirty bytes, 128 connections, two connections per
neighbor, and two-page windows (`cmd/racer-dataplane/src/config.rs:175-186`).
Application divides aggregate dimensions by worker count, preserving neighbor
and window caps (`src/app.rs:229-248`). Thus each sampled worker has 128 MiB
plaintext/ciphertext, 64 MiB dirty, and 64 connections. Seven complete
16 MiB-plus-tag ciphertext pages fit, not eight. Request timeout is 30 seconds
and reader-stall timeout 10 seconds (`src/config.rs:167-169`).

The SDK fixture already uses the exact eight variable layer sizes and four
simultaneous continuations. Originally ingress had seven ciphertext-page charges
while serving peers had 64 (`src/peer/production_stream_tests.rs`, node builder).
Its external SDK context is 80 seconds and driver scope 100 seconds; real client
listener requests use 30 seconds. It has four members, one worker per member,
one SDK ingress, and no concurrent SDK clients on serving nodes. This is not the
1500-member graph with every node simultaneously pulling, serving, and relaying.
All abbreviated `src/` paths in this section are relative to `cmd/racer-dataplane`.

## Experimental tests and actual results

These are diagnostic experiments, not demonstrated regression tests for the live
bug. Only test code was changed. They are retained uncommitted for the parent to
reuse; they do not justify a fix or a fix commit.

### Uniform serving-peer memory

Added `sdk_full_image_with_uniform_peer_memory_pressure` in
`cmd/racer-dataplane/src/peer/production_stream_tests.rs`. It extends the existing
fixture with the ingress seven-page ciphertext budget on all serving peers,
retaining two neighbor slots and the existing full-image assertions.

Command:

```sh
timeout 40s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib sdk_full_image_with_uniform_peer_memory_pressure -- --ignored --nocapture
```

Result: **PASS**, warm and cold images, each manifest/config/eight layers with
542,950,400 verified layer bytes. Initial build 28.41 seconds; test 6.53 seconds.
Three additional runs, each under `timeout 12s` with a bounded 38-second Python
driver, returned **[0, 0, 0]**, six more complete images. No causal failure.

All four SDK variants were then run concurrently:

```sh
timeout 22s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib sdk_ -- --ignored --nocapture --test-threads=4
```

Result: **4 passed**, eight complete warm/cold images, 7.07 seconds. These are
independent graphs, not four pullers sharing the same graph. Printed
`client connection: Err(Io)` also occurs with successful byte/digest assertions
after SDK connections close; it is not evidence of the live error.

### Two-worker continuation handoff

Extended the existing production test Rig to accept a shared WorkerDirectory
and worker ID. Added
`two_worker_bootstrap_and_concurrent_continuations_verify_bytes` in
`cmd/racer-dataplane/tests/production_dataplane.rs`.

It creates separate thread-local production read/storage/crypto graphs sharing
one two-worker directory, verifies bootstrap, then four concurrent pinned
remainders of a `4 * 16 MiB + 113` byte object. An assertion verifies that the
five pages span both owners. Exact response bytes are checked. It uses a real
Unix HTTP origin adapter and direct client HTTP, not the Go SDK or peer serving.

```sh
timeout 38s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --test production_dataplane two_worker_bootstrap_and_concurrent_continuations_verify_bytes -- --nocapture
```

Initial result: **PASS**, 0.60 seconds after compilation. Adding the ownership
assertion initially used nonexistent `StrongEtag::new`, a test compile error;
corrected to the existing `StrongEtag::parse` API. The same test under
`timeout 25s` then **passed with both-owner assertion**, 0.60 seconds. This is
evidence against a simple handoff failure, not against a coupled fleet failure.

Both edited Rust files were formatted with bounded `rustfmt --edition 2024`.
No lint/full-suite/build-image claim is made.

## Final mechanism review and blocker

Paths below are relative to `cmd/racer-dataplane`.

- Handoffs transfer the original budget and return remaining credits
  (`src/read/dispatch.rs:342-354`); start_page seals a charged context and submits
  an owned command (`:457-482`). Cross-thread charge release uses atomic counters
  (`src/runtime/admission.rs:67-72`). No premature-release defect was established.
- Production peer ingress exists only on the control worker
  (`src/app.rs:881-888`), dispatching page work to its actual owner
  (`src/read/dispatch.rs:536-558`). The passing single-worker SDK fixture bypasses
  this coupled two-worker peer/client arrangement.
- Pool return requires completed framing and exclusive FD ownership
  (`src/http/pool.rs:124-133,151-175`). Idle reclamation drops only an idle owned
  lease (`:415-440`); peer acceptance uses that pool (`src/peer/server.rs:75-86`).
  No exact production FD race was found.
- Fill reserves progress memory before peer I/O and waits within the existing
  deadline if live owners retain it (`src/read/fill.rs:419-438,497-516`). Peer
  body receive consumes the pre-reserved output (`src/peer/transfer.rs:401-429`),
  and serving retains ciphertext through response I/O
  (`src/peer/server.rs:279-295`). Coupled client/serving waits remain possible
  hypotheses, not observed wait cycles.
- Candidate attempts use the tighter original deadline, not independent shorter
  per-candidate deadlines (`src/read/flight.rs:164-170`,
  `src/read/candidates.rs:294-330`). An attempt consuming that deadline propagates
  terminally (`src/read/candidates.rs:182-192`). Live chosen routes, candidate
  errors, and remaining budgets were not observed, so the stall cannot be
  attributed to a particular route or deadline.
- Replay capacity is shared node-wide and does not retain ciphertext pages
  (`src/security/replay.rs:21-50,105-109`). No live replay rejection was observed.
- `Event::Overload` exists in `src/telemetry/metrics.rs:25,59`, but source search
  found no production recording call site for it. Diagnostic HTTP counters record
  diagnostics, not page failures. A zero overload counter cannot exonerate page
  or connection admission.

**Blocker:** the observed client-visible first-page stall and previous settled
EOFs cannot be linked to an internal continuation, page acquisition, or peer
failure using existing logs/counters. The focused local scenarios all pass old
production code. No old-fail/new-pass or concrete live root cause is established.

**Specific parent request:** provide a bounded diagnostic capture or parent-owned
diagnostic deployment on one sampled ingress and its implicated peers. Correlate
SDK bootstrap/continuation start and terminal status with dataplane request/page,
ingress/owner worker, peer destination/route, remaining deadline/credits, and
terminal error. At a stalled memory/slot wait, include resource class and
used/limit plus owner category (client window, peer send/receive, or persistence).
Instrument the currently discarded terminal result at
`src/client/listener.rs:179-182` and the error returned from
`src/client/response.rs:112-117`; correlate with
`src/read/fill.rs:427-438` and `src/peer/transfer.rs:378-415`. Record no payloads,
authorization headers, tokens, or key material. An uncorrelated socket-count
snapshot would not distinguish these causes.

Stop pending that evidence. No production edit, bootstrap change, deployment,
quota change, or commit was performed. No contract change is proposed.

## Final handoff checks

Base remains `03c240def2eb357d19e36639137d1a1cedc41f8b` on
`racer-fix-live-restarts`. Worktree is intentionally **dirty and uncommitted**:
two diagnostic test files modified and this report untracked. Ignored local
probe/evidence files remain in `tmp/`. No production code changes or commit.

Commands and results in the final handoff-only phase:

```sh
timeout 5s rustfmt --edition 2024 --check cmd/racer-dataplane/src/peer/production_stream_tests.rs cmd/racer-dataplane/tests/production_dataplane.rs
```

**PASS**, no output. No further Rust edits were needed.

```sh
timeout 40s cargo clippy --manifest-path cmd/racer-dataplane/Cargo.toml --release --all-targets -- -D warnings
```

**FAIL**, existing warnings promoted to errors: library compilation reported 58
errors, library-test compilation 74. Examples in unchanged files include
`src/client/transition.rs:89` (`collapsible_if`), `src/control/secrets.rs:16`
(`type_complexity`), and `src/read/dispatch.rs:66` (`large_enum_variant`). No
Clippy recipe was found in the searched Makefile/README/CI YAML files. The prior
settled-pulls report explicitly retained existing Clippy warnings. Strict lint
failure is preserved, not reported as a pass or resolved by unrelated edits.

To let Clippy reach all test targets despite existing library warnings:

```sh
timeout 25s cargo clippy --manifest-path cmd/racer-dataplane/Cargo.toml --release --all-targets --message-format=json -- -W warnings
```

Output was piped to a bounded 26-second Python JSON consumer that printed
diagnostics touching either edited file and the `build-finished` status.
**Completed successfully**, 9.18 seconds, 133 warning diagnostics across target
builds. No diagnostic pointed to newly added code. The only warning in either
edited file was the unchanged nested `if` in
`tests/production_dataplane.rs:618-623`; `git diff` confirmed that block was not
edited. This warning-mode completion does not supersede the strict lint failure.

```sh
timeout 40s cargo build --manifest-path cmd/racer-dataplane/Cargo.toml --release
```

**PASS**, 12.83 seconds including waiting for Clippy's build-directory lock.
Default features only; no image build or deployment. `git diff --check` also
passed. Previously passing diagnosis tests were not rerun in this handoff phase.

## Authorized live uprobes: new causal evidence

The parent subsequently authorized targeted read-only uprobes using existing
net-node access. Earlier scripts were inspected in sibling worktrees:
`racer-fix-settled-pulls/tmp/{trace,inspect}.py`,
`racer-fix-fleet-admission/tmp/trace.py`, and
`racer-fix-fleet-layers/tmp/trace-wire.py`. New scripts and evidence remain in
this worktree's ignored `tmp/`; no remote file was written.

Live tooling: bpftrace v0.9.4, nm, readelf, objdump, addr2line. Each trace runs
under remote `timeout -k 2s 18s`, with a ten-second bpftrace exit probe, remote
Python alarm 27 seconds, exec timeout 28 seconds, explicit kubectl request
timeout 20 seconds and outer `timeout 30s`, local subprocess timeout 31 seconds.
Local tracing invocations used `timeout 35s` and shell timeout 36 seconds.
HTTP requests use 15-second read timeouts. Probes wait for `Attaching` before
issuing HTTP. Selected headers are decoded in memory; payloads and credentials
are not retained. Hashing request bindings retains only the resulting digest.

### Fresh ELF verification and failures retained

`tmp/live-disasm.log` establishes ingress PID 333275 and ELF virtual addresses:
Admission::reserve_inner `0x173ac0`, overload branch `0x174284`. Executable
segment virtual address exceeds file offset by 4096, so uprobe offsets are
`0x172ac0` and `0x173284`. The branch writes error discriminant 13. Disassembly
identifies resource class in r13, amount in r15, limit at sp+8, and aggregate
usage through Admission+160. Resource class 7 is Connection.

- `live-trace-1.json`: bpftrace newline escaping error; no successful attachment.
  The foreground GET still ran in this early version. Not causal uprobe evidence.
- `live-trace-2.json`: bpftrace v0.9.4 printf argument-limit error; the attachment
  gate prevented the HTTP probe. Removed one printed field, retaining TID in maps.
- `live-trace-3.json`: trace collection timed out because join was too short.
  The bounded tracer self-terminated; a separate pgrep confirmed no bpftrace.
- `live-trace-4.json`: successful ingress attachment and exit 0, no admission
  failure events. Cleanup pgrep exit 1 and empty output mean no matching process.

### Foreground ingress: actual premature EOF

Trace 4 foreground GET began at Unix **1790437733.6340098**. Gantry returned
HTTP 200 and Content-Length **68,935,168**. The reader received **16,777,216**
bytes, then `read()` returned empty with no timeout/error after **0.061565947 s**.
That is a short-body EOF, not success or a client timeout.

Within this GET window, target layer page 1 had three Acquire attempts sharing
request ID **`ozClC9x3WD8WP3TQujA8Ww==`**:

| Request Unix time | Next-hop IP | Response Unix time | Outcome | Links / remote attempts |
| --- | --- | --- | --- | --- |
| 1790437733.6809385 | 10.243.19.77 | 1790437733.6875126 | overloaded | 4 / 3 |
| 1790437733.6887310 | 10.245.113.78 | 1790437733.6928022 | overloaded | 8 / 2 |
| 1790437733.6940136 | 10.241.226.87 | 1790437733.6948085 | overloaded | 4 / 0 |

Trace 4 matches requests/responses by flow/order and time; it predates binding
computation. The selected key and foreground time window strongly associate the
attempts with the GET, but no end-to-end SDK request ID was captured. Zero
ingress admission failures does not prove absence of other ingress errors.

### Relay node and resource source

Fresh Kubernetes mapping: `10.241.226.87` is `racer-dataplane-pb862`, node
`aks-ddv5-17198779-vmss0000ae`, net-node `unbounded-net-node-fmckr`, image
`03c240def2eb357d19e36639137d1a1cedc41f8b`. Its peer identity is
`952ee656-b916-493e-b6c4-92d38f9192dd`. PID is **861501**. Fresh
`tmp/unbounded-net-node-fmckr-disasm.log` confirms identical admission offsets.

Trace 5 records 81 Connection admission failures, each amount 1, used 64, limit
64, caller virtual address `0x183be8`, symbolized to
`HttpPool::reserve_connection`. Aggregate trace map identifies TID 861501,
the control/ingress worker, not worker 1. Its foreground probe timed out after
15 seconds at 16 MiB; this is not the earlier 61 ms EOF experiment.

`tmp/peer-pool-inspection.log` disassembles reserve_connection and accept.
Virtual address `0x183d55` is the no-idle-entry result of reclamation. Trace 6
adds that point (file offset `0x182d55`) and the caller of reserve_connection:
50 no-idle failures from accept (`0x1844c9`) and 12 from outbound checkout
(`0x185146`), all TID 861501. Thus completed outbound idle reclamation was
actually attempted and found no candidate; this is not merely an initial
reservation failure that later recovered.

The adjacent /proc socket snapshot shows 64 established incoming peer sockets,
63 with empty transmit/receive queues, and one with 0xca5 transmit bytes. Three
outbound peer sockets were CLOSE_WAIT with one receive byte. This snapshot does
**not** assign every socket to a worker or prove that the 63 empty sockets were
application-idle: they could be waiting on page work or another peer. It must not
be called proof of idle-header waits or a dependency deadlock.

### Exact request-binding match and timed checkout failure

Trace 7 computes the binding from the full decoded signed request in memory,
following `cmd/racer-dataplane/src/security/signing.rs:235-301`: sorted covered
components, RFC 9421 signature base, domain separator, lengths, Ed25519 signature,
SHA-256. The response's `racer-request-binding` matches the computed digest.
This is exact binding equality, not a new independent signature verification.

Evidence: `tmp/unbounded-net-node-fmckr-trace-7.json` and
`tmp/binding-correlation.json`, generated by `tmp/correlate-bindings.py`.

- Background source flow: **10.244.227.87:32894 -> 10.241.226.87:8082**.
- Target layer `515e6f5a...`, **page 2**, Acquire, destination
  `21f9bcdc-8c0b-4c4e-a65b-305d743bdef0`, links 4, remote attempts 3.
- Request ID **`umWmRNPQx7GrlNO+0NCw/A==`**;
  attempt **`Zn7U+RSszKRgwHWRf9bi8A==`**.
- Request observed **1790438204.4565334**.
- Outbound checkout NOIDLE on **TID 861501**, **1790438204.4571128**;
  connection amount 1, used/limit 64/64 in the paired admission event.
- Matching signed-envelope overloaded response **1790438204.4575102**.
- Exact binding **`QZB8qc0qQmqHycd5QJFEy+fNG/ZZy4xBxAtRbs54dhc=`**.

This proves a binding-matched target-layer page request receives overloaded with
an outbound connection failure in its sub-millisecond processing window. The
uprobe itself has no request ID, so assigning that checkout to the request is
temporal same-worker attribution rather than a direct request-tagged probe.
It is a **background page-2 flow**, not the foreground page-1 EOF request from
trace 4. Do not join different phases into one exact end-to-end request.

Code maps relay `Overloaded` to a signed overloaded response
(`src/peer/server.rs:321-327`), and acquisition handles candidate overloads before
returning Unavailable when candidates are exhausted
(`src/read/candidates.rs:168-208`). Gantry aborts a failed continuation
(`internal/gantry/mirror/racer.go:135-147`). These explain the observed propagation;
the SDK/dataplane terminal Error enum was not directly probed.

## Focused old-code failure and precise approval decision

Added one mechanism test, not a production fix:
`cmd/racer-dataplane/src/http/pool_peer_tests.rs`:
`completed_incoming_keepalives_do_not_block_outbound_progress`.

It fills an unchanged two-connection test quota with real accepted TCP sockets,
receives actual HTTP requests, sends zero-length responses, and successfully
calls finish_exchange. Both connections are then polled into pending next-header
receives. A new peer checkout returns Overloaded. Cleanup cancels and fences
pending I/O and asserts connection quota returns to zero before the final
progress assertion. Here application-idleness is known by construction.

```sh
timeout 35s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib completed_incoming_keepalives_do_not_block_outbound_progress -- --nocapture
```

**OLD FAIL**, 0.01 seconds after compilation:
`completed incoming HTTP keepalives blocked outbound progress`,
left `Some(Overloaded)`, right `None`. No new-pass exists. This demonstrates the
missing incoming-idle reclamation mechanism, but does not independently prove
the live sockets were in the same idle state. It is not a full-image peer-relay
regression. The initial test's unused import was removed; bounded rustfmt and
rustfmt check passed for that file, and
`timeout 25s cargo check --manifest-path cmd/racer-dataplane/Cargo.toml --release --tests`
passed (6.32 seconds). No passing test result is claimed for this intended failure.

**Approval requested:** permit registering completed accepted-peer keepalives as
reclaimable idle and canceling plus fencing a pending next-header receive under
outbound pressure, while preserving active exchanges and any partial-header
exchange. No quota increase, automatic request retry, or change to normal
checkout's fail-fast exhaustion semantics. The existing distinction between
checkout and checkout_peer's bounded neighbor-slot wait remains.

Smallest expected production scope: `src/http/pool.rs` for bounded idle-peer
registration/reclamation signaling, `src/http/io.rs` for exact first-byte versus
idle-receive state and completion fencing, and `src/peer/server.rs` for registering
only completed peer keepalives. Tests must cover partial-head races, active
responses, cancellation fences, quota release, and progress. Existing runtime
fence APIs should be reused; no runtime change is justified yet. Relevant prose
is `src/http/INTEGRATION.md:59-71` and `src/peer/INTEGRATION.md:34-51`.
All these src paths are relative to `cmd/racer-dataplane`.

Remaining required evidence: directly observe live idle-header ownership or
reproduce the coupled peer-relay/full-image failure with known idle accepted
owners, then obtain old-fail/new-pass for the approved implementation. Socket
queue emptiness alone is insufficient. No lifecycle change was implemented.

## Cleanup and current handoff

Successful traces 4, 5, 6, and 7 exited 0 and recorded pgrep exit 1 with empty
output. Final independent checks on **both** `unbounded-net-node-kwgql` and
`unbounded-net-node-fmckr` used explicit context/request timeout and bounded
`nsenter ... pgrep -a -x bpftrace`; both exited **1, no matches**. No bpftrace
process remains on either host.

Scripts: `tmp/inspect-live.py`, `tmp/inspect-pool.py`, `tmp/trace-live.py`,
`tmp/correlate-bindings.py`, plus earlier `tmp/probe-one-layer.py`.
Raw files: `tmp/live-disasm.log`, `tmp/live-trace-{1,2,3,4}.json`,
`tmp/unbounded-net-node-fmckr-disasm.log`, `tmp/peer-pool-inspection.log`,
`tmp/unbounded-net-node-fmckr-trace-{5,6,7}.json`,
`tmp/binding-correlation.json`, and `tmp/live-one-layer.json`.

Base remains `03c240def2eb357d19e36639137d1a1cedc41f8b`; worktree is dirty with
three diagnostic test files and this report. `git diff --check` passed. No
production modification, deployment, quota change, lifecycle change, or commit.

## Approved implementation in progress

The user approved pressure reclamation of completed accepted-peer keepalives.
Candidate code adds a bounded weak idle registry, a non-consuming readiness wait
between peer exchanges, and a completion-owned readiness lease. The connection,
registration, and quota remain in the reactor's operation until its fence even
if the future is abandoned. Partial header bytes already queued at the peek
exclude reclamation. Arrival after the empty peek races with pressure closure;
the documented cancellation-wins behavior consumes no bytes. Parent cancellation
now has an explicit operation-owned waker subscription. Normal checkout remains
fail-fast; checkout_peer may wait for its selected idle cancellation to fence.

`incoming_idle_preserves_partial_heads_and_fences_cancel_races` passes active,
queued-partial, completed readiness/full head, parent cancellation (including a
real wake-count assertion), drop, deadline, and arrival-after-reclaim cases.
Connection quota is asserted before and after fencing without relying on a
closed numeric FD remaining unused. Global-capacity waiters are distinguished
from neighbor-slot waiters to avoid waking the latter on unrelated capacity.

Exact old/new mechanism check uses the same updated test and peer idle API.
Only the `checkout_peer -> checkout_inner` incoming-reclamation argument was
temporarily switched from true to false, then immediately restored:

- `tmp/incoming-idle-old-fail.log`: test fails, `Some(Overloaded)` versus `None`.
- `tmp/incoming-idle-new-pass.log`: restored candidate passes.
- Both use `completed_incoming_keepalives_do_not_block_outbound_progress`.
- No permanent baseline toggle remains: checkout_peer passes true, checkout false.

The dedicated SDK case `sdk_full_image_reclaims_accepted_peer_keepalives` is
still under construction and currently fails. It seeds real PeerServer challenge
exchanges, retains completed accepted keepalives, and requires a nonzero incoming
reclamation count. Its initial 60 idle waits exposed the fixture's sixteen-entry
reactor queue before SDK start. The dedicated variant now uses the observed live
two-worker partition of 128 operations for 64 connections, leaving other variants
at sixteen operations. Subsequent 60- and 56-keepalive configurations still reject
SDK client acceptance during bootstrap/layer setup; neither is a passing causal
full-image test. Logs: `tmp/incoming-idle-sdk-candidate-{2,3,4}.log`; the initial
compile-error log is `tmp/incoming-idle-sdk-candidate.log`. The file named
`incoming-idle-sdk-new-pass.log` also contains an early failed setup, not a pass.
Read contents, not filenames, when evaluating results.

Next required fixture change is to establish SDK ingress before introducing
accepted-peer pressure, so client acceptance does not mask the outbound mechanism.
No SDK old-fail/new-pass is claimed yet. No commit or deployment has occurred.

## Causal SDK regression completed

The setup problem was resolved with a test-only Go/Rust barrier. Four actual SDK
layer streams consume bootstrap on established connections, then signal a local
file. Rust fills the exact remaining connection quota with completed real
PeerServer challenge exchanges through a dedicated seed TCP listener, avoiding
the concurrently polled production listener's accept. Rust releases the barrier
and polls the retained peer keepalive tasks while the SDK continues. The test
requires a nonzero incoming reclamation count. Other SDK variants do not enable
the barrier or pressure scenario. No production quota was changed.

An earlier barrier attempt failed because the normal pending accept consumed the
seed socket (`tmp/incoming-idle-sdk-barrier-1.log`). Dedicated-listener candidate
passes both warm and cold full images (`tmp/incoming-idle-sdk-barrier-2.log`).

For the exact causal comparison, only the incoming-reclamation boolean passed
by checkout_peer was temporarily disabled. The same test and full graph then
failed (`tmp/incoming-idle-sdk-old-fail.log`): pinned continuations returned
Unavailable and layers ended at 16,777,216 bytes instead of 68,935,168,
59,679,232, and 59,757,056; SDK reported I/O errors. Reclamation was immediately
restored, with no permanent toggle.

After crate rustfmt and Go-fixture gofmt, the restored candidate passed again:

```sh
timeout 32s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib sdk_full_image_reclaims_accepted_peer_keepalives -- --ignored --nocapture
timeout 15s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib http:: -- --test-threads=1
timeout 15s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib peer::server:: -- --test-threads=1
```

Results: SDK **PASS**, warm and cold manifest/config/eight layers with exact
lengths and SHA-256, 542,950,400 layer bytes each; HTTP **28 passed**; peer server
**6 passed**. Logs are `tmp/incoming-idle-sdk-restored-pass.log`,
`tmp/incoming-idle-http-tests.log`, and `tmp/incoming-idle-peer-server-tests.log`.
The SDK case ran in 6.79 seconds after compilation; HTTP 4.67 seconds; server
0.01 seconds. The first SDK old-code run used `timeout 30s`.

This proves the selected incoming-idle starvation mechanism and fix locally,
including the same first-page failure boundary. It does not retroactively prove
every empty live socket was application-idle or every fleet EOF had this cause.
No cluster deployment or fleet recovery claim is made. Final reactor checks,
changed-code lint review, and commit review remain outstanding.
