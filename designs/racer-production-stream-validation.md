# Fill output reservation in production SDK peer streams

Base: `0b9d6770b8e8bf1367e4f54d077ff5a8123ccd03`. Worktree:
`tmp/racer-fix-production-stream`, branch `racer-fix-production-stream`.
One new read-only OpenCode investigator was launched for this task, session
`ses_f23736adbffeYYLcbDNwjVf5yM`. No other agents were launched.

## One established bug

Fill atomically reserves plaintext and ciphertext progress before acquisition
(`cmd/racer-dataplane/src/runtime/admission.rs:208-229`). It retains the ciphertext
reservation while resolving candidates (`src/read/fill.rs:427-442,497-516`, paths
relative to `cmd/racer-dataplane` below). At the base, production HTTP peer receive
independently reserved another ciphertext buffer (`src/peer/transfer.rs:365-385`
at the base). The first reservation was unused on peer success. Active Fill
reservations cannot be reclaimed as idle cache entries.

The real SDK issues a bootstrap followed by one pinned remainder request
(`pkg/racersdk/value.go:117-142`, repository-relative). RangeStream runs a bounded
window of page acquisitions concurrently (`src/read/range_stream.rs:174-215`).
Four layers with two-page windows can therefore reserve the available output
capacity and then reject healthy peer pages that should consume that capacity.
Candidate failures consume finite original credits (`src/read/candidates.rs:134-193`
at the base). Late failures truncate successful HTTP responses
(`src/client/response.rs:109-118`); early transient failures map to 503 (`:148-156`).

The fix passes the optional existing output reservation through CandidatePolicy
and Requester to Transfers. HTTP body admission validates owner/cache/class/size,
then moves the charge into WireBuffer and onward into the decoded page. Empty
responses and pre-body failures leave the reservation with Fill. Submitted I/O
retains it through the reactor completion fence. If a failed body consumed it and
placement authorizes origin fallback, Fill reacquires the output within the
original deadline. The post-origin-412 copy search also receives the output.
Limits, placement, wire protocol, retry credits, and persistent formats are unchanged.

## Fresh read-only fleet correlation

All cluster commands explicitly selected `joolshev-scale-test`, request timeout
20s, subprocess cap 30s. Full-image probes had 110s alarms and 120s outer caps;
traces had 22s remote alarms, 24s remote command caps, and 40s outer caps. Only
existing hostPID net-node pods were used. Temporary uprobes detached automatically.
No AKS configuration or durable state, host sysctl, or main-worktree code changed.

Exact fleet image:
`loadgen.invalid/benchmark/image@sha256:2aa1e8745512f4f76499b8b9a5543642630d3f1120668d8636278537d9aa53a6`.
Load remains one image per node, four concurrent layers, eight 64 MiB-base layers
with 20 percent jitter. Dataplane, Gantry, and loadgen were each 1500/1500 ready;
the dataplane DaemonSet image was the exact base.

Evidence is retained under this worktree's ignored `tmp/`:

- `production-stream-settled.json` and `.md`: 0/3 full images, 17 successful
  layers, six premature EOFs, one layer 503. Stable sampled UIDs/restarts and
  process starts; full-pull error deltas 9, 10, 10, success deltas zero.
- `production-stream-final-settled.json` and `.md`: 0/3 full images, 11 successful
  layers, ten premature EOFs, three layer 503s. Full-pull error deltas 9, 10, 11,
  success deltas zero. Prometheus had 1500/1500 coverage, 8195 cumulative successes
  and 7,580,370 errors. Its five-minute rates are not controlled causal comparisons.
- `unbounded-net-node-82j5x-disasm.log`: inspected the actual running `0b9d` binary.
  Reserve entry ELF address `0x270c40`, overload branch `0x271166`; file offsets
  are 0x1000 lower. The resource class is the low byte of BP at the failure branch,
  requested bytes R15, configured limit R13. Do not reuse earlier binaries' offsets.
- `unbounded-net-node-6tcfq-wire.json`: simultaneous selected-header/uprobes on
  `10.242.226.74`, node identity `45915147-e2bd-4342-a0b1-902f3a21b9d3`.
  There are 34 receive-reservation failures, including initial and reclamation
  retries, and 7385 reserve-fill failures. These are not distinct failed requests.
  Receive failures request 16,777,232 bytes against a 134,217,728-byte worker cap.
  Callers resolve to WireBuffer::for_cache (`0x1ad7b7`) and receive_buffer's initial
  and retry sites (`0x1afa21`, `0x1afa89`).

The latter trace correlates a **locally originated** acquisition, rather than only
transit traffic:

1. Event 17 at Unix time 1790406982.8041134: page 1 of layer `2115d07a...408c1d`,
   request `PpImXy2VL+fzx1R2g1jS9Q==`, attempt `cVKbMvcdROTe9PMlK0sRfw==`.
   Its visited list contains only the traced node. It uses Acquire to candidate
   `aede9a49-7e8d-4f78-9062-c2d3de968acd` through `10.241.25.75:8082`, local port
   41272, with zero delegated attempts remaining.
2. Event 21 at 1790406982.951377: the reverse path returns a signed page descriptor
   advertising 16,777,232 ciphertext bytes, binding
   `32CT1yYBIr/OEqIjdSNRY2JuLk2PzR/wNA8fcu73sqw=`.
3. Initial receive reservation fails at approximately 1790406982.951723, and the
   retry after reclamation at 1790406982.951747. Event 22 at 1790406982.9522445 is
   RST on that same socket. This local acquisition follows the Fill path holding
   its unused advance reservation. Moving that reservation would avoid this
   additional local reservation for the advertised page.

The explicit GET in the same trace independently returns HTTP 200, expected
59,757,056 bytes, but EOF at 16,777,216 bytes in 0.1864s. It is a different request
from the background correlation above. Uprobes do not record request IDs; attribution
uses the socket, selected fields, caller, and clock-converted ordering. Captures
can duplicate/drop/reorder packets and do not independently verify signatures.
No credentials or page payloads are retained. This establishes a real avoidable
failure on the production acquisition path, not exclusivity for every fleet error.

## Causal old-fail/new-pass and full-image coverage

`sdk_sliding_range_uses_fill_receive_capacity` in
`src/peer/production_stream_tests.rs` builds the actual Go SDK fixture using an
overlay. It drives four independent production admission/read graphs, real TCP
peer servers, Requester, handshake, signed forwarding, placement, Fill/Flights,
WorkerDirectory, page crypto, memory, O_DIRECT slab setup, client HTTP parsing,
RangeStream, Responses, and delivery over an actual Unix listener.

Every layer page's placement excludes the ingress node. Thus the SDK continuation
must use requester-side Fill plus real peer receive. The SDK reads four bootstrap
pages, then synchronizes the four remainder streams. The ingress quota is seven
full ciphertext pages (117,440,624 bytes), below the observed fleet worker's
128 MiB cap. Other graph limits isolate that ingress pressure. Two runs cover
candidate caches prewarmed through elected real Fill, and cold candidate Fill.

Both runs transfer and hash a fixture manifest and config, parse their descriptors,
then verify all eight complete layers with **the fleet's exact layer lengths**,
542,950,400 total layer bytes. They use deterministic fixture payloads and therefore
different digests from the fleet image. They are not the exact fleet OCI artifact.
Final assertions require zero plaintext, ciphertext, dirty, relay, and connection
charges after drain.

With only Fill's new handoff restored to the old `resolve_with_budget` call,
the final fixture fails: five layers truncate at 16 or 32 MiB with Unavailable and
SDK I/O errors. Restoring `resolve_reserved(..., &mut ciphertext)` passes both
warm and cold full images at identical limits. Earlier eight-charge and layer-only
fixtures also failed old/passed new; the seven-charge full-image fixture is the
final acceptance result.

Additional tests check wrong owner/cache/class/size, malformed lengths without
consuming the output, real fragmented and truncated HTTP receive with charge
transfer, and origin fallback after consumed peer output, including cancellation.
The four earlier full-image peer regressions pass too.

Precise boundaries: the polling loop and origin byte/metadata adapter are fixtures;
there is no Application executable startup, controller enrollment, Gantry, OCI
extraction, or persistence/recovery acceptance in the new test. Four members are
not a sparse 1500-node topology or one simultaneous image on every fleet node.
Native RDMA retains its existing receive path and was not hardware-validated.
No new kind cluster was attempted; the retained single-dataplane kind cluster
cannot establish this multi-peer cause. Post-fix AKS recovery remains unverified.

## Checks and affected image

All builds/tests used explicit outer caps at most 300s; no indefinite polling.

```sh
timeout 300s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml \
  --locked --release --all-features --lib sdk_sliding_range -- --ignored --nocapture
timeout 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml \
  --locked --release --all-features --lib full_image_ -- --ignored --nocapture
```

Release all-feature suite: 559 library, two binary, 18 conformance, nine
production-graph, and 31 doc tests pass; 13 library opt-ins are skipped by default.
The new SDK and four full-image opt-ins pass separately. Cargo fmt/check and
all-target/all-feature Clippy pass; preexisting lint warnings remain, including
the already-large CandidatePolicy request argument list. Scoped make fmt/lint,
actionlint, and SDK Go tests were run. Formatter-only changes outside the intended
files were reverted.

The production Containerfile build passed with a 300s cap. Local image:
`racer-dataplane:production-stream`, OCI index
`sha256:3e5c4c7df95e6f4d7c68c33b00633233f41de79f1b522c0bb19cc35632e8cb29`.
It was built before committing, with default revision label `unknown`.
Only the **racer-dataplane** production image needs rebuilding from the resulting
commit for publication with the correct revision label. No image is pushed and no
AKS rollout is performed by this task.
