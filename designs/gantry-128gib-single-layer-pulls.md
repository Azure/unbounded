# Gantry 128 GiB Single-Layer Pull Limitations

**Status:** Open tracking document

## Evidence Scope

This document starts from the known behavior of Gantry `v0.8.0` when pulling
an OCI image with one approximately 128 GiB layer, then tracks the remediation
stack against the current repository. It also records relevant behavior from
containerd `v2.3.5`, the version selected by this repository. Measurements
from a production pull are not yet available; all throughput thresholds below
are arithmetic derived from the configured sizes and deadlines.

The chair configuration controls cold-start coordination. Values such as
`chair_lease_duration`, `chair_seed_count`, and
`chair_cluster_size_estimate` do not control how long an origin response body
may download.

## Summary

Gantry `v0.8.0` has two independent absolute duration limits on origin pulls:

1. Every registry HTTP request has a five-minute `http.Client.Timeout`
   (`internal/gantry/origin/origin.go:364`). This includes reading the entire
   response body.
2. A chair's background origin pull starts with a five-minute budget, extends
   that budget using the advertised content size, and clamps the result to 30
   minutes (`cmd/gantry/main.go:1851-1897`).

The five-minute HTTP client timeout is the first effective limit in `v0.8.0`.
A 128 GiB layer must average at least about 437 MiB/s from origin through Gantry
to finish inside it. Increasing only the background budget cannot extend the
inner HTTP client deadline.

PR 1 removes that HTTP total deadline. The 30-minute detached-pull ceiling
remains until PR 2. Gantry also does not perform ranged origin pulls, so
interrupted origin downloads do not have an efficient end-to-end resume path.

## Data Paths

### Live mirror request

The local containerd requests a digest from Gantry. Gantry may stream an origin
response directly back to containerd without writing a second local Gantry
copy (`cmd/gantry/main.go:542-548`,
`internal/gantry/mirror/mirror.go:1127-1182`). In `v0.8.0`, the origin HTTP
client's five-minute total timeout remains active while containerd reads the
body. PR 1 removes that total deadline.

### Chair background pull

A selected chair downloads the digest from origin into its local containerd
content store, commits it, and advertises it to peers
(`cmd/gantry/main.go:1846-2005`). This path is subject to both the HTTP client
timeout and the size-aware outer budget.

## Quantitative Boundaries

Assuming 128 GiB means 131,072 MiB and ignoring protocol, authentication,
storage, and commit overhead:

| Limit | Required average throughput |
|---|---:|
| 5 minutes | 436.9 MiB/s |
| 30 minutes | 72.8 MiB/s |

The values are derived as:

```text
131,072 MiB / 300 s   = 436.9 MiB/s
131,072 MiB / 1,800 s = 72.8 MiB/s
```

Estimated transfer durations at constant payload throughput are:

| Throughput | Derived transfer time |
|---|---:|
| 10 MiB/s | 3 h 38 m 27 s |
| 25 MiB/s | 1 h 27 m 23 s |
| 50 MiB/s | 43 m 41 s |
| 100 MiB/s | 21 m 51 s |
| 250 MiB/s | 8 m 44 s |
| 500 MiB/s | 4 m 22 s |

The background budget calculation uses a 10 MiB/s throughput floor plus five
minutes, but then clamps the result to 30 minutes. For 128 GiB, the unclamped
budget would exceed three hours and is reduced to 30 minutes. The registry
HTTP client still ends the active response at five minutes.

## Issue Inventory

The inventory separates blockers for an uninterrupted progressing 128 GiB
pull from follow-up work that reduces the cost of an interruption. An issue is
**Open** until its owning PR is merged and its acceptance checks pass.

### Progressing-pull blockers

| ID | Issue | Evidence | Owning PR | Status |
|---|---|---|---|---|
| L1 | Registry HTTP requests have a five-minute total timeout, including body reads. | [Gantry v0.8.0 origin client](https://github.com/Azure/unbounded/blob/v0.8.0/internal/gantry/origin/origin.go#L351-L365) | PR 1 | Addressed |
| L2 | Detached chair origin pulls have a 30-minute absolute ceiling after size adjustment. | `cmd/gantry/main.go:1851-1897` | PR 2 | Open |
| L3 | Detached pulls have no operator-configurable no-progress timeout. The existing settings are absolute duration limits, which are not the desired policy. | `internal/gantry/config/config.go` | PR 2 | Open |

Resolving L1-L3 is sufficient for a continuously progressing 128 GiB layer to
run beyond five and 30 minutes. It does not make interrupted transfers
resumable.

### Interruption efficiency

| ID | Issue | Evidence | Owning PR | Status |
|---|---|---|---|---|
| L4 | Gantry origin pulls do not send `Range` and cannot request an origin body from an offset. | `internal/gantry/origin/origin.go:25`, `internal/gantry/origin/origin.go:477-542` | PR 4 | Open |
| L5 | A failed background ingest is aborted. A later writer with a stale nonzero offset is also aborted because callers restart at byte zero. | `internal/gantry/containerdstore/store.go:328-383`, `internal/gantry/containerdstore/store.go:502-519` | PR 5 | Open |
| L6 | Gantry does not handle the local containerd client's inbound `Range` header on the origin path. | `internal/gantry/mirror/mirror.go:685-930` | PR 4 | Open |

L4-L6 do not cause the fixed five- or 30-minute failures. They determine
whether a transfer interrupted for another reason can continue without
downloading its prefix from origin again.

### Observability

| ID | Issue | Evidence | Owning PR | Status |
|---|---|---|---|---|
| L7 | Logs expose a context deadline, but do not identify the deadline owner, bytes transferred, elapsed time, or whether the pull was live or detached. | `cmd/gantry/main.go:1963-1977`, `internal/gantry/mirror/mirror.go:1151-1160` | PR 3 | Open |

### Constraints, not open Gantry issues

- Once Gantry has returned `200` and started a response body, it cannot replace
  that response with a `5xx` to force containerd onto the next registry host.
  Recovery must happen by completing, resuming, or failing the current fetch.
- Containerd's `image_pull_progress_timeout` is a no-progress timeout. It is
  not a total-duration limit and does not replace Gantry-owned stall detection
  for detached chair pulls.
- Efficient resume depends on the origin registry honoring byte ranges. Gantry
  must report restart-from-zero behavior when the origin does not support them.

## Retry And Fallback Behavior

### Gantry background retries start over

The background pull copies origin bytes into a content writer. Any copy error
returns without committing, and the deferred abort removes that ingest. When a
later writer finds a partial ingest, Gantry deliberately aborts it because the
origin client always streams from byte zero. Gantry therefore does not resume
the previous background pull.

### Containerd has a limited offset retry path

Containerd `v2.3.5` wraps registry bodies in `httpReadSeeker`. It tracks bytes
read and can reopen at the current offset after `io.ErrUnexpectedEOF`
([httpreadseeker.go](https://github.com/containerd/containerd/blob/v2.3.5/core/remotes/docker/httpreadseeker.go#L28-L90),
[reopen path](https://github.com/containerd/containerd/blob/v2.3.5/core/remotes/docker/httpreadseeker.go#L145-L178)).

That behavior does not make the current Gantry path resumable:

- The timeout error observed from Go's `http.Client.Timeout` is a context
  deadline error, not necessarily `io.ErrUnexpectedEOF`. The internal reopen
  path is therefore not guaranteed to run.
- On a reopen, containerd sends `Range: bytes=<offset>-` to its first configured
  pull host, which is Gantry.
- Gantry ignores that range on its mirror origin path and starts a new origin
  request at byte zero.
- If a host returns `200` and ignores the range, containerd can discard bytes
  locally until it reaches the old offset
  ([resolver.go](https://github.com/containerd/containerd/blob/v2.3.5/core/remotes/docker/resolver.go#L700-L729)).
  This avoids duplicate bytes in the destination but repeats origin transfer
  of the prefix.

An efficient resume requires Gantry to preserve and forward the requested
offset and the origin registry to honor byte ranges.

### Origin fallback is not guaranteed after a partial response

The generated containerd host configuration puts Gantry first and the
registry server after it. Containerd can select the next host when Gantry is
unreachable, returns an unusable status before body transfer, or otherwise
fails to open the response (`deploy/gantry/hosts.toml.template:11-17`).

The five-minute failure happens while reading a body after Gantry has already
returned a successful response. This is not equivalent to Gantry returning a
pre-body `5xx`. Containerd may fail the fetch or retry Gantry; direct origin
fallback for that partially transferred request is not guaranteed.

### Containerd's five-minute timeout has different semantics

Containerd `v2.3.5` also defaults `image_pull_progress_timeout` to five minutes,
but it is an inactivity timeout, not a total pull duration. It resets whenever
new image bytes are read. A continuously progressing 128 GiB transfer may run
longer than five minutes without triggering it
([config.go](https://github.com/containerd/containerd/blob/v2.3.5/internal/cri/config/config.go#L43-L63),
[configuration semantics](https://github.com/containerd/containerd/blob/v2.3.5/internal/cri/config/config.go#L328-L335),
[progress monitor](https://github.com/containerd/containerd/blob/v2.3.5/internal/cri/server/images/image_pull.go#L686-L738)).

Increasing Gantry's total timeout does not inherently conflict with
containerd's progress timeout. Containerd will continue the pull while bytes
make progress. It does not provide a reliable escape from Gantry's shorter
absolute deadline.

## Configuration Notes

There is no supported Gantry ConfigMap setting for L1 or L2 in `v0.8.0`. PR 1
removes L1 rather than making its absolute duration configurable. L2 remains
hardcoded until PR 2.

The valid chair field names are:

```yaml
chair_claim_initial_divisor: 2048
chair_cluster_size_estimate: 100000
```

The spellings `chair_cliam_initial_divisor` and
`chair_cluster_size_esitmate` are invalid. Gantry loads YAML with known-field
checking, so those misspellings in an actual ConfigMap should fail startup
rather than tune the intended values (`internal/gantry/config/config.go:543-549`).

## Delivery Plan

The preferred behavior is progress-bounded transfer rather than an absolute
duration selected independently of content size. Each PR below owns one
behavioral boundary and can be reviewed and validated independently.

### PR 1: Remove the origin HTTP total deadline

**Purpose:** Allow a live or detached origin response body to remain open while
it is progressing.

- [x] Remove the five-minute `http.Client.Timeout`.
- [x] Use an HTTP transport with bounded connection establishment, TLS
  handshake, and response-header waits.
- [x] Preserve caller cancellation. The live path remains bounded by the local
  containerd request context; the detached path remains temporarily bounded by
  its existing 30-minute budget until PR 2 lands.
- [x] Add origin-client tests showing that body duration is not limited by a
  client-wide deadline and that connection/header phases remain bounded.

**Addresses:** L1.

**Explicitly out of scope:** detached-pull policy, new configuration, Range,
partial-ingest resume, and fallback behavior.

**Dependencies:** None.

### PR 2: Make detached pulls progress-bound

**Purpose:** Replace the chair background pull's 30-minute total duration with
a Gantry-owned no-progress policy.

- [ ] Add a body-progress watchdog to `runOriginPull` that resets after
  successful reads.
- [ ] Remove the five-minute size formula and 30-minute ceiling.
- [ ] Add an `origin_pull_idle_timeout` setting to YAML, environment variables,
  flags, validation, and the shipped ConfigMap. Define zero explicitly.
- [ ] Test that a progressing detached body may exceed the former ceiling and
  that an inactive body is canceled after the configured interval. Use
  injected timing or shortened test durations.

**Addresses:** L2 and L3.

**Explicitly out of scope:** live-path Range handling and retaining partial
background ingests.

**Dependencies:** PR 1, so the HTTP client does not fire before the progress
watchdog.

### PR 3: Identify timeout and transfer failures

**Purpose:** Make field failures attributable without changing transfer
semantics.

- [ ] Log the pull mode (`live` or `detached`), deadline owner, expected size,
  bytes transferred, and elapsed time.
- [ ] Add bounded-cardinality metrics for deadline owner and pull mode.
- [ ] Distinguish caller cancellation, connection/header timeout, body idle
  timeout, and downstream writer failure.

**Addresses:** L7.

**Explicitly out of scope:** changing timeout values, retry behavior, or Range
support.

**Dependencies:** PR 1 and PR 2 establish the final deadline sources that this
PR reports.

### PR 4: Resume local containerd retries through Gantry

**Purpose:** Avoid replaying the origin prefix when local containerd retries a
live Gantry response at a nonzero offset.

- [ ] Validate a single inbound blob range from local containerd.
- [ ] Carry the offset through `ifaces.OriginRef` into the origin request.
- [ ] Require and validate the origin's `206` and `Content-Range` response.
- [ ] Return matching range semantics to containerd.
- [ ] Preserve request-scoped delegated registry authorization on the origin
  retry and never forward it to a peer.
- [ ] Test supported ranges, ignored ranges, malformed ranges, and a mid-body
  interruption followed by an offset retry.

**Addresses:** L4 and L6.

**Explicitly out of scope:** retaining a chair's partial background ingest.

**Dependencies:** None for correctness; landing after PR 1 keeps timeout and
resume failures easier to distinguish during review.

### PR 5: Resume detached chair ingests

**Purpose:** Reuse bytes already staged in containerd when a detached chair
pull is interrupted.

- [ ] Expose a verified writer offset through the local content-store
  abstraction.
- [ ] Reopen the origin at that offset using PR 4's origin range support.
- [ ] Continue the existing containerd ingest and rely on commit-time digest
  verification over the complete layer.
- [ ] Abort and restart from byte zero when the origin cannot honor the range,
  and log that decision.
- [ ] Test process-local retry, stale partial state, unsupported ranges, digest
  mismatch, and delegated authorization.

**Addresses:** L5.

**Explicitly out of scope:** cross-node partial transfer and chunk-level
distribution.

**Dependencies:** PR 4.

### PR 6: End-to-end compatibility validation

**Purpose:** Validate the combined behavior against a real containerd client
and registry without introducing another production behavior change.

- [ ] Cover pre-header Gantry failure and confirm containerd selects its next
  configured registry host.
- [ ] Cover mid-body interruption and confirm containerd retries with an
  offset that Gantry serves without replaying the prefix.
- [ ] Stream a large logical body without allocating a 128 GiB fixture.
- [ ] Run a measured 128 GiB single-layer pull and record elapsed time,
  throughput, retries, origin bytes, and final digest verification.

**Addresses:** validation for L1-L6; it does not own an issue by itself.

**Dependencies:** PR 1, PR 2, PR 4, and PR 5. PR 3 is recommended so failures
are attributable.

Making only the five-minute value configurable is not a planned PR. It would
leave operators calculating a total duration from image size and expected
throughput, while the independent 30-minute ceiling and restart-from-zero
behavior remained.

## Acceptance Criteria

### Required for progressing 128 GiB pulls

- A continuously progressing 128 GiB layer completes through Gantry even when
  total transfer time exceeds five and 30 minutes.
- A body that makes no progress is canceled after the configured idle timeout.
- Connection, TLS handshake, and response-header stalls remain bounded.

### Required for interruption efficiency

- An interrupted origin transfer resumes from the verified stored offset when
  the registry supports byte ranges; the already transferred prefix is not
  downloaded from origin again.
- When resume is unsupported, logs state that the transfer restarted from byte
  zero.
- Containerd host fallback behavior is covered by an integration test for both
  pre-header failure and mid-body interruption.
- Delegated registry authorization remains request-scoped across retries and
  is never exposed to peer endpoints.
- Tests generate or stream large logical bodies without allocating a 128 GiB
  fixture in memory or on disk.

## Evidence Still Needed

For the reported two-node cluster, collect the following before attributing
every deadline to L1:

- The complete Gantry log entry surrounding the deadline. In particular,
  distinguish `origin pull failed`, `origin pull copy failed`, and
  `mirror: live origin stream failed`.
- Elapsed time and bytes transferred before each failure.
- Whether the request was the local containerd live stream or a chair's
  background pull.
- Effective origin-to-Gantry and Gantry-to-containerd throughput.
- Whether the registry returns valid `206` and `Content-Range` responses for
  blob range requests.
- The local containerd `image_pull_progress_timeout` and any kubelet or CRI
  request deadline configured outside Gantry.