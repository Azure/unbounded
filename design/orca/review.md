<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Orca code review and remediation plan

This document records a code-review pass over `internal/orca/` and
`cmd/orca/`, and a remediation plan for the issues found. Findings are
classified by severity; the plan groups them into tiers from
must-fix-before-production to nice-to-have cleanups.

This version incorporates corrections from an adversarial review pass
(see "Review history" at the end).

The review is point-in-time. As code changes, individual line numbers
will drift; the descriptions are intended to be specific enough that
the underlying issue stays identifiable.

---

## Prerequisite refactor

Several bug fixes depend on the same plumbing: the `fetch.Coordinator`
needs to know the authoritative object size when filling and serving a
chunk. Today it only knows `k.ChunkSize` and `k.Index`, which is
sufficient for non-tail chunks but does not let the leader (a) detect
a short-body origin response, (b) clamp `GetChunk`'s requested length
on the tail chunk, or (c) set an authoritative `Content-Length` on the
internal-fill response.

### P0. Plumb `info.Size` through fetch + cluster

**Scope:** `internal/orca/fetch/fetch.go`, `internal/orca/cluster/cluster.go`, `internal/orca/server/server.go` (chunk-key construction), `internal/orca/inttest/` (test seams as needed).

**Description:** The edge handler already has `info.Size` from `HeadObject` (`server.go:110`). The fetch coordinator's `GetChunk` API takes `chunk.Key` only. Extend the chunk-key carrying path so the leader knows the expected last-chunk size. Options:

1. Add `ObjectSize int64` to `chunk.Key` (cleanest; ObjectSize is part of the chunk's identity contract since it determines the tail-chunk length).
2. Pass `info.Size` as a separate argument through `GetChunk`/`fillLocal`/`runFill` (intrusive but avoids changing `Key`).

`Key.Path()` already encodes `ChunkSize` in the hash; adding `ObjectSize` would change the encoding and invalidate previously cached chunks. So option 2 is safer for the prototype: extend the in-process API without touching the on-the-wire chunk-key encoding. The internal-fill RPC (`encodeChunkKey` / `DecodeChunkKey`) gains an `object_size` query parameter that the leader uses to compute expected length and reject short bodies.

**Sequencing:** Land P0 before any of B1, B4, B7 - all three depend on it.

---

## Findings

### Confirmed bugs (correctness)

#### B1. Origin response shorter than expected -> catalog records short length, subsequent reads under-deliver
**Location:** `internal/orca/fetch/fetch.go` - `runFill`, the `io.Copy(buf, body)` step, and the catalog record on success.

**Description (revised):** `runFill` asks `fetchWithRetry` for `length = k.ChunkSize` bytes from origin and unconditionally `io.Copy`s the response into `buf`. If origin returns fewer bytes than expected:

- `cachestore/s3.PutChunk` is called with `size = int64(buf.Len())` (the actual body length), so the cachestore itself is consistent with what was committed (`s3.go` validates `size == len(buf)` against its own re-read - tautological in this call).
- The catalog records `cachestore.Info{Size: int64(buf.Len())}` on `Record`. That is the *short* length.
- Subsequent `GetChunk` calls on the catalog-hit path pass `k.ChunkSize` to `cs.GetChunk`, not `info.Size`. The S3 GET against a range past EOF returns either a short body (LocalStack) or 416 (real AWS). Either way, the edge handler's `streamSlice` calls `io.CopyN(dst, src, length)` with `length` computed from `ChunkSlice(info.Size)` - if the actual object is shorter than `info.Size` suggested, the copy returns `io.ErrUnexpectedEOF` mid-stream.
- Joiners in the same singleflight (reading `f.bodyBuf.Bytes()`) receive the same short bytes regardless.

So the bug is real but not what was originally described. The shape is *catalog* poisoning (under-recorded length, then trusted by the cachestore-hit fast path), plus joiners getting truncated data.

**Fix (requires P0):** After `io.Copy(buf, body)`, validate `buf.Len() == expectedLen(k, objectSize)` where `expectedLen` is `min(k.ChunkSize, objectSize - off)`. On mismatch: treat as a retryable origin error; do not call `cs.PutChunk` and do not `Record` the catalog. Also update `cs.GetChunk` callers on the hit path to pass the actual expected per-chunk length (not `k.ChunkSize` blindly) so that even a short object served via cachestore-hit is bounded correctly.

**Risk if left:** A flaky origin under-delivers; orca permanently caches the short result; clients see truncated bodies on subsequent reads.

---

#### B2. `metadata.Cache.LookupOrFetch` singleflight stale-entry race
**Location:** `internal/orca/metadata/metadata.go` - leader's deferred close-and-delete.

**Description:** Current defer order is `close(sfe.done)` then `c.sf.Delete(k)`. A second caller arriving between those two calls does `c.sf.LoadOrStore(k, ...)`, gets the stale entry whose `done` is already closed and whose `once` has been consumed, and silently returns `sfe.info` / `sfe.err` without ever calling `fetch`. This is most dangerous when `recordResult` took the "transient error -> not cached" branch: the transient error is replayed to the joiner with no retry.

**Fix:** Swap the defer order: `c.sf.Delete(k)` *before* `close(sfe.done)`. A new caller arriving after `Delete` creates a fresh entry and runs `fetch`; existing joiners that already loaded the old pointer still read the result via the closed `done`.

**Concurrency note:** The fix introduces a brief window where one caller has the old entry (about to read the result) and another caller has just done `LoadOrStore` and gotten a fresh entry (about to run a new fetch). For a moment both the old leader's fetch result and the new caller's fresh fetch can be in flight for the same key. This is *not* a correctness bug - the new caller will run a real fetch and either confirm the previous result or discover updated state. But it does mean a worst-case duplicated HEAD per miss-completion under contention. Cluster-wide dedup via the rendezvous coordinator mitigates this further. Acceptable; document.

**Risk if left:** Rare but real transient-error replay under load; hard to reproduce in test but can manifest as flapping 502s.

---

#### B3. DNS error wipes the good peer-set with self-only
**Location:** `internal/orca/cluster/cluster.go` - `refresh`.

**Description:** Current code:
```go
peers, err := c.source.Peers(ctx)
if err != nil || len(peers) == 0 {
    self := []Peer{{IP: c.cfg.SelfPodIP, Self: true}}
    c.peers.Store(&self)
    return
}
```
A transient DNS error or one-tick empty result overwrites a known-good multi-peer snapshot with `[Self]`. For at least one refresh interval (5 s in prod) every chunk's rendezvous coordinator becomes Self, undoing cluster-wide dedup and causing a wave of unwanted local fills.

**Fix:** On `err != nil`:

- **If a previous non-empty snapshot exists** in `c.peers`: retain it (do not store). Log + increment a metric `cluster_refresh_error_total` so persistent DNS failure surfaces.
- **If no previous snapshot exists** (bootstrap case, `c.peers.Load() == nil`): apply the `[Self]` fallback (same as today).

On `len(peers) == 0` with `err == nil`: this is a legitimate "I'm alone" answer; apply the `[Self]` fallback as today.

**Staleness ceiling:** After N consecutive errors (N = `5` initially, configurable), even with a previous snapshot, fall back to `[Self]`. This bounds how long we route to dead peers if DNS is permanently broken. The peer-side internal-fill RPC failure already falls back to local fill (`fetch.go:154-160`), so brief dead-peer routing is tolerable, but unbounded staleness is not.

**Risk if left:** Coordinator thrash on transient DNS hiccups; observable as brief origin GET amplification.

---

#### B4. `WriteHeader` committed before first chunk fetched -> silent truncation looks like success
**Location:** `internal/orca/server/server.go` - `handleGet`, the `WriteHeader(statusCode)` call before the first `GetChunk`.

**Description:** Headers (`200 OK` / `206 Partial Content` + `Content-Length: N`) are committed before chunk 0 is fetched. If chunk 0's cold fill fails after retries, the handler logs a warn and `return`s. Clients see `200 OK\r\nContent-Length: N\r\n\r\n` followed by a short body or connection RST. Clients that check Content-Length will catch this; many will not.

**Fix (requires P0 to compute expected length per chunk):**

1. Fetch the first chunk's reader before committing headers. On the cold path the reader is a `*bytes.Reader` over `f.bodyBuf`, so peek is trivial. On the cachestore-hit path the reader is an HTTP body; a `bufio.Reader.Peek(1)` proves origin reachability without buffering more than 1 byte.
2. If the peek errors, call `writeOriginError` and return normally (no headers committed).
3. Once the peek succeeds, commit headers and stream the rest.
4. For mid-stream failures on chunks 1..N: panic with a sentinel error type recovered at the handler boundary so the HTTP server resets the connection (HTTP/1.1) or the stream (HTTP/2) rather than appearing as a clean close. Do *not* use `http.Hijacker` - it is not implemented under HTTP/2.

**Verification:** B4 cannot be unit-tested with `httptest.ResponseRecorder` because Recorder does not model write-after-WriteHeader stream truncation. Use `httptest.NewServer` and assert client-side that an io.ErrUnexpectedEOF (or stream-reset) is observable, not a clean EOF + Content-Length mismatch silently passed.

**Risk if left:** Silent truncation; clients consume bad data without any error signal.

---

#### B5. Azure `If-Match` header quoting (NEEDS VERIFICATION)
**Location:** `internal/orca/origin/azureblob/azureblob.go` - `Head` strips ETag quotes; `GetRange` wraps unquoted in `azcore.ETag(etag)` and sets `IfMatch`.

**Description:** Azure requires the `If-Match` header value to be quoted on the wire. The current code strips quotes in `Head` (`strings.Trim(string(*props.ETag), "\"")`) and passes the unquoted value back through `azcore.ETag(etag)` in `GetRange`. **If** the SDK's `azcore.ETag` type does not re-add the quotes when marshalling, the precondition silently never fires.

The Azure SDK's `azcore.ETag` is `type ETag string`; the typed conditional-access fields in `azblob` are conventionally quoted on the wire by the SDK marshaller, but this needs explicit verification (e.g. a unit test that captures the outbound HTTP `If-Match` header value, or a manual `tcpdump` against Azurite). Until verified, treat this as a potential issue rather than a confirmed bug.

**Fix (after verification):** If the SDK does not re-quote: pass through the quoted form (do not strip in `Head`), or re-wrap explicitly: `azcore.ETag("\"" + etag + "\"")`. If the SDK does re-quote: no code change; remove this finding.

**Tier:** B5 is moved to Tier 2 pending the verification test.

**Risk if left:** If the SDK does not re-quote: ETag-changed-mid-flight goes undetected on Azure origins, with the same cache-poisoning consequences as B1.

---

#### B7. `cluster.FillFromPeer` does not validate the peer body length
**Location:** `internal/orca/cluster/cluster.go` - `FillFromPeer` returns `resp.Body` directly; `internal/orca/server/server.go` - `InternalHandler.ServeHTTP` does `io.Copy(w, body)` without setting `Content-Length`.

**Description:** The internal-fill response has no `Content-Length`. If the connection drops mid-body the requesting replica's downstream `io.Copy` sees EOF and returns a short body to the client. No length check anywhere on the cross-replica hop.

**Fix (requires P0):**

1. The leader's `InternalHandler.ServeHTTP` sets `Content-Length` on the response. This requires the leader to know the chunk's authoritative length on both the cold-fill path (where `f.bodyBuf` is already materialized - trivially `buf.Len()`) and the cachestore-hit path (where the length is `min(k.ChunkSize, objectSize - off)` - computable from `objectSize` once P0 plumbs it through, or by calling `cs.Stat(k)` if not).
2. `FillFromPeer` wraps `resp.Body` in a counting reader that, at EOF, errors if the counted bytes don't equal `resp.ContentLength`.
3. The internal-fill handler can stream chunked-by-chunked once Content-Length is set; no need to buffer the full chunk before responding (the cachestore-hit path was already a stream).

**Risk if left:** Silent truncation across the cross-replica hop. Same shape as B4 but on the internal listener.

---

### Reclassified findings

#### B6 (was Tier 1, now Tier 3). `DecodeChunkKey` does not validate `chunk_size > 0`
**Location:** `internal/orca/cluster/cluster.go` - `DecodeChunkKey`.

**Description (revised):** The internal-fill code path with `chunk_size = 0` reaches `cs.GetChunk(ctx, k, 0, k.ChunkSize)` which becomes a 0-byte range request - not a crash. The edge-handler division paths (`chunk.IndexRange`, `chunk.ChunkSlice`) are *not* reached from the internal handler. So a buggy peer with `chunk_size=0` causes a 0-byte response, not a divide-by-zero crash.

This is still input-validation hygiene worth doing - defense in depth - but the original "buggy peer can crash a replica" risk is overstated.

**Fix:** Validate `chunkSize > 0` and `index >= 0` in `DecodeChunkKey`; return an error decoded as 400 on the wire.

**Tier:** Demoted to Tier 3.

---

#### B8 (was Tier 2, now Tier 4 docs). `azureblob.List` and `awss3.List` are consistent
**Location:** `internal/orca/origin/azureblob/azureblob.go`, `internal/orca/origin/awss3/awss3.go`.

**Description (revised):** Both drivers return a single page per call and surface a `NextMarker` for caller-driven pagination. The contract is consistent across drivers today; the earlier framing of "inconsistency" was wrong.

**Fix:** Document the per-page semantics in `internal/orca/origin/origin.go`'s `List` interface comment. No code change.

**Tier:** Demoted to Tier 4 (docs only).

---

### Correctness concerns (acceptable tradeoffs, document)

#### C1. `runFill` detached from request context
**Location:** `internal/orca/fetch/fetch.go` - `runFill` constructs its own `context.WithTimeout(context.Background(), 5*time.Minute)`.

**Description:** If every caller disconnects, the leader keeps pulling from origin for up to 5 minutes, pinning an `originSem` slot. The 5-minute cap bounds it. Acceptable for MVP because the bytes may still benefit future callers; document in `design.md`.

---

#### C2. `commit-after-serve` failure serves bytes but does not record
**Location:** `internal/orca/fetch/fetch.go` - the `else` branch where `commitErr` is neither `nil` nor `ErrCommitLost`.

**Description:** On non-`ErrCommitLost` `PutChunk` errors the bytes are still served to in-flight joiners (good - bytes are correct), but the catalog is not updated. The next request misses and re-fills. Worth a metric (`commit_after_serve_failed_total`) so persistent cachestore degradation surfaces in monitoring.

---

#### C3. `countingResponseWriter` does not pass through `http.Flusher`
**Location:** `internal/orca/inttest/internalwrap.go`.

**Description:** Today applied only to the internal handler, which does not type-assert `Flusher`. Live behaviour is fine. Tripwire: reuse on the edge handler (which does flush per chunk) would silently disable streaming.

**Fix:** Implement `Flush()` on the wrapper via type assertion on the embedded `ResponseWriter`. Same for `Hijacker`/`CloseNotifier` if any future handler needs them.

---

### Missing findings (added from review)

#### M1. No explicit cap on concurrent in-flight fills
**Location:** `internal/orca/fetch/fetch.go` - `c.inflight` map.

**Description:** `f.bodyBuf` is held in `c.inflight[path]` until `runFill` returns. With 8 MiB chunks and N concurrent requests for distinct keys, memory usage scales as N x 8 MiB. The per-replica origin semaphore (`target_per_replica`) is the actual cap on concurrent fills today - so peak buffer footprint is `target_per_replica * chunk_size`. With defaults of 64 / 8 MiB that's ~512 MiB on a single replica under full saturation.

**Fix:** Document the math in `design.md`. Optionally add a `fills_inflight` gauge metric (current `len(c.inflight)`) so operators can see saturation. No structural code change strictly required.

**Tier:** Tier 2 (metric + docs).

---

#### M2. `app.Wait` drops listener errors on ctx-first
**Location:** `internal/orca/app/app.go` - `Wait`.

**Description:** `Wait` selects on `ctx.Done()` and `errCh`. If ctx fires first, `Wait` returns nil even if `errCh` has a pending listener error. Benign for "serve until SIGTERM" but loses signal for diagnostics.

**Fix:** After `ctx.Done()`, drain `errCh` non-blockingly and log any pending errors before returning.

**Tier:** Tier 3.

---

### Code quality

#### Q1. Dead branch in `cluster.IsCoordinator`
**Location:** `internal/orca/cluster/cluster.go` - the `coord.IP == c.cfg.SelfPodIP && coord.Port == 0` fallback after the `coord.Self` check.

**Description:** Verified: every code path that produces a `coord` value stamps `Self` correctly (`dnsPeerSource` matches by `selfIP`; `StaticPeerSource` stamps by `(selfIP, selfPort)`; the empty-peer-set fallback constructs `c.self()` which sets `Self: true`).

**Fix:** Remove the fallback.

---

#### Q2. Dead-defensive type-assertion error returns
**Location:** `internal/orca/chunkcatalog/chunkcatalog.go` (`Lookup`, `Record`); `internal/orca/metadata/metadata.go` (`lookup`, `recordResult`).

**Description:** The package fully controls what goes into these lists/maps. The type assertions cannot fail. The error returns and corresponding caller checks add noise.

**Fix:** Direct type assertion (`x.(*entry)`); drop error returns; simplify call sites.

---

#### Q3. Typo `skipCacheSelfTst`
**Location:** `internal/orca/app/app.go` - field name in `options`.

**Fix:** Rename to `skipCacheSelfTest`.

---

#### Q4. Dead import-guard variables in `server.go`
**Location:** `internal/orca/server/server.go` - the trailing `var (_ = cachestore.ErrNotFound; _ = context.Canceled)` block.

**Description:** Comment claims this "survives dead-code elimination". Neither is used elsewhere in the file; the `cachestore` import is otherwise unused; `context` is used for `context.Context` types. Both lines + the `cachestore` import can go.

**Fix:** Delete both `_ = ...` lines and the `cachestore` import.

---

#### Q5. `cachestore/s3.PutChunk` double-buffers chunks
**Location:** `internal/orca/cachestore/s3/s3.go` - `PutChunk` does `io.ReadAll(r)` even when `r` is an `*bytes.Reader`.

**Description:** Callers pass `bytes.NewReader(buf.Bytes())` which implements `io.ReadSeeker`. The SDK can use it directly. Current code unconditionally reads it all into a fresh byte slice -> two copies of the chunk in memory during the put. With 8 MiB chunks and concurrent fills this is meaningful pressure.

**Fix:** Type-assert `r.(io.ReadSeeker)`; if it is, hand it to `Body` directly. `io.ReadAll` only as a fallback for non-seekable readers.

---

#### Q6. `fetch.fetchWithRetry` does not check `ctx` at top of loop
**Location:** `internal/orca/fetch/fetch.go` - `fetchWithRetry`, loop body.

**Description:** Backoff sleep checks `ctx.Done()`. Initial attempt does not. A pre-cancelled context still issues a `GetRange` (which usually fails fast, but wastes a round trip).

**Fix:** `if err := ctx.Err(); err != nil { return nil, err }` at the top of the loop body.

---

#### Q7. `cluster.Close()` not ctx-aware
**Location:** `internal/orca/cluster/cluster.go` - `Close`.

**Description:** Blocks on `<-c.done`. If `refresh` is mid-DNS-lookup with the 3-second internal timeout, `Close` waits up to 3 s after the caller signaled shutdown.

**Fix:** Accept a `context.Context` on `Close(ctx)` so callers can cap.

---

#### Q8. `app.WithEdgeListener` / `WithInternalListener` undocumented production-impact
**Location:** `internal/orca/app/app.go`.

**Description:** These options bypass `cfg.Server.Listen` and `cfg.Cluster.InternalListen` bind paths. Intended for tests but nothing structurally prevents production use.

**Fix:** Add a comment block marking them as test-only seams.

---

#### Q9. Inconsistent error mapping helpers across origin drivers
**Location:** `internal/orca/origin/awss3/awss3.go` vs `internal/orca/origin/azureblob/azureblob.go`.

**Description:** Both drivers translate SDK errors to `origin.ErrNotFound` / `origin.ErrAuth` / typed errors, but the helpers differ. Not a bug, but a new driver implementer has no single reference for the contract.

**Fix:** Add a comment block in `internal/orca/origin/origin.go` enumerating which external condition maps to which sentinel/typed error.

---

### Simplifications

#### S1. `cluster.Resolver` interface now only used internally
After removing `WithResolver`, the `Resolver` type is referenced only by `dnsPeerSource`. Could be unexported (`resolver`) with `net.DefaultResolver` referenced directly. Minor.

#### S2. `app.options.clusterOpts` is a slice but only ever holds one option
Since only `cluster.WithPeerSource` is ever pushed today, the slice could be a single-value field.

---

## Remediation plan

### Tier 0: prerequisite plumbing

0. **P0** plumb `info.Size` from edge handler down through `fetch.Coordinator` and the internal-fill RPC. Necessary for B1, B4, B7.

### Tier 1: must-fix before production

Address before any production rollout. These are silent-correctness hazards.

1. **B2** swap defer order in `metadata.LookupOrFetch` (one-line fix); document the new (benign) concurrent-fetch window.
2. **B3** preserve previous peer set on DNS error with a bootstrap special-case and a max-staleness ceiling.
3. **B1** validate origin body size in `runFill` against `min(ChunkSize, objectSize - off)` and treat short body as retryable; on the cachestore-hit path, clamp `GetChunk`'s requested length to the actual chunk size for the tail.
4. **B7** internal-fill `Content-Length` plus a counting reader in `FillFromPeer`.

### Tier 2: should-fix soon

5. **B4** restructure `handleGet` to peek the first chunk's reader before `WriteHeader`; on mid-stream failures panic-to-reset the connection. Verification via `httptest.NewServer`, not Recorder.
6. **B5** verify Azure `If-Match` wire quoting via a captured outbound HTTP header; fix only if confirmed broken.
7. **Q5** `PutChunk` seekable-reader passthrough.
8. **M1** document the concurrent-fill memory math; add a `fills_inflight` gauge metric.

### Tier 3: cleanup (low risk, high signal)

9. **B6** validate `chunkSize > 0` / `index >= 0` in `DecodeChunkKey`.
10. **Q1** remove dead branch in `IsCoordinator`.
11. **Q2** remove dead-defensive type-assertion error returns in `chunkcatalog` and `metadata`.
12. **Q3** rename `skipCacheSelfTst` -> `skipCacheSelfTest`.
13. **Q4** delete `_ = cachestore.ErrNotFound` / `_ = context.Canceled` import guards and the now-unused `cachestore` import in `server.go`.
14. **Q6** ctx-check at the top of `fetchWithRetry`'s loop.
15. **Q7** ctx-aware `cluster.Close(ctx)`.
16. **Q8** mark `WithEdgeListener` / `WithInternalListener` as test-only.
17. **Q9** add origin-sentinel mapping comment block to `internal/orca/origin/origin.go`.
18. **C3** `countingResponseWriter` implements `Flusher` (and `Hijacker`/`CloseNotifier`).
19. **M2** drain `errCh` after `ctx.Done()` in `app.Wait`.

### Tier 4: design notes (no code change)

20. **C1** runFill detached context - document the 5-minute timeout choice in `design.md`.
21. **C2** commit-after-serve failure path - document the no-record behavior; consider adding a metric in a future revision.
22. **B8** document the per-page-per-call `List` semantics in `origin.go`.
23. **S1** / **S2** simplification opportunities (unexport `Resolver`, single-value `clusterOpts`) - noted, not urgent.

---

## Sequencing recommendation

- **PR 0**: P0 only. The `info.Size` plumbing refactor in isolation, with no behavior change. Reviewed cleanly before any of the bug fixes land on top.
- **PR 1 (Tier 1 + Tier 3)**: bundle the must-fix correctness issues with the low-risk cleanups. Most cleanups touch the same files (`cluster.go`, `fetch.go`, `metadata.go`, `server.go`) as the Tier 1 fixes, so reviewing them together is cheap.
- **PR 2 (Tier 2)**: B4 (`handleGet` restructure) + B5 (Azure verification) + Q5 + M1. The B4 work is the most substantial; benefits from being reviewed on its own.
- **PR 3 (Tier 4)**: design-doc updates capturing C1 / C2 / B8 / S1 / S2.

---

## Verification gate per change

For each Tier 0 / 1 / 2 / 3 item, before considering it landed:

- The narrowest test that would have failed before the fix exists and passes after.
- `make` is green (lint + unit tests + build).
- `make orca-inttest` is green.
- For mid-stream truncation changes (B4, B7): use `httptest.NewServer` (not `httptest.ResponseRecorder`) so the test models real HTTP write-after-WriteHeader truncation. Assert client-side that the failure is observable (io.ErrUnexpectedEOF, stream reset, or Content-Length mismatch error) - not a clean EOF.
- For B5: assert outbound `If-Match` header value matches Azure's expected wire format (quoted) via an inttest fake or by capturing the request in a test HTTP server.
- For B1: deliberately short the LocalStack response (or use a fault-injection origin decorator) and verify the leader rejects + retries rather than committing the short body.

---

## Review history

This document was generated in a code-review pass on the orca packages
and then reviewed adversarially. The adversarial review found 15
issues with the initial plan; this version incorporates the
corrections:

- **B1**'s explanation was reworded - catalog poisoning, not cachestore short-write. Also extended to cover the hot-path `GetChunk` length-clamping requirement.
- **B2**'s fix now documents the new (benign) concurrent-fetch window.
- **B3**'s fix adds a bootstrap special-case and a max-staleness ceiling.
- **B4**'s fix drops the `http.Hijacker` suggestion (incompatible with HTTP/2) and specifies `httptest.NewServer` for verification.
- **B5** moved from Tier 1 to Tier 2 pending verification (the original plan classified it as confirmed despite explicitly saying "needs verification").
- **B6** demoted to Tier 3 - the divide-by-zero crash path is not reachable from the internal listener as originally claimed.
- **B7**'s fix is scoped to require P0 plumbing.
- **B8** reclassified to docs-only Tier 4 - the original "inconsistency" claim was wrong; both drivers are single-page-per-call.
- **M1** added: in-flight fill memory math (capped by origin semaphore today, but worth a metric + doc).
- **M2** added: `app.Wait` drops listener errors on ctx-first.
- New **P0** tier added for the `info.Size` plumbing prerequisite shared by B1, B4, B7.
- **Q1**'s "dead branch" claim verified by the reviewer.

Adversarial-review verdict: "ship with corrections."

---

## Second-pass findings and remediation

A second review pass over the orca packages turned up additional
issues and led to 12 follow-up commits.

### Landed findings

The following findings were identified and fixed in the second pass.
The naming convention re-starts (B-1 through B-11, etc.) since the
first pass already used B1-B8 with different meanings; readers should
disambiguate by surrounding text.

- **B-1 (block-blob and versioning gates locked unconditionally).**
  `config.applyDefaults` used `if !X { X = true }` for two booleans
  (`EnforceBlockBlobOnly`, `RequireUnversionedBucket`). The shape
  implied operators could opt out, but the code actually overrode
  user-set `false` back to `true`. Removed both fields from
  config; drivers always enforce. YAML now ignores both keys (clean
  break: operators who set them will fail to parse).
- **B-2 (zero-byte object served 416).** Edge handler computed
  `rangeEnd = info.Size - 1 = -1`, then fell into the
  `rangeStart > rangeEnd` guard, returning 416 for a normal GET on
  an empty file. Added an explicit size==0 short-circuit; Range
  requests against zero-byte objects remain 416 per RFC 7233.
- **B-7 (Azure If-Match unquoted).** `azureblob.GetRange` passed the
  internal unquoted ETag straight to `azcore.ETag` for `If-Match`.
  Now re-wraps the value in quotes at egress, mirroring the awss3
  driver. RFC 7232 requires quoted-strings; Azure tolerated unquoted
  values in practice but the contract was inconsistent across
  drivers.
- **B-9 (60s wall timeout on cross-replica HTTP client).**
  `cluster.newHTTPClient` carried `Client.Timeout: 60s` which
  aborted any internal-fill body stream exceeding the budget
  (plausible for 8 MiB chunks on degraded links). Removed the wall
  clock; caller ctx (edge request ctx or `fetch.runFill`'s detached
  fill ctx) is the sole deadline.
- **B-3 / B-4 / B-6 (cachestore/s3 error mapping).** Three related
  bugs: `isPreconditionFailed` matched `"InvalidArgument"` and
  `"ConditionalRequestConflict"` plus `strings.Contains(err.Error(),
  "412")`; `mapErr` 5xx detection was `strings.Contains(err.Error(),
  "StatusCode: 5")`; a vestigial `_ = http.StatusOK` kept the
  `net/http` import alive. All three replaced by
  `*awshttp.ResponseError`-based HTTP status code inspection.
- **Q-10 (awss3 mirror of the above).** Same string-matching
  fragility in the origin driver. Same fix.
- **O-4 (slog.Default in fetch.Coordinator).** Coordinator hardcoded
  `slog.Default()` for peer-fallback warnings and
  commit-after-serve traces, preventing operators from routing
  fetch-path logs alongside the rest of the runtime. Injected
  `*slog.Logger` through `NewCoordinator`.
- **O-2 (no kubelet probe endpoints).** Added a third HTTP listener
  bound to `cfg.Server.OpsListen` (default `0.0.0.0:8442`, plain
  HTTP, no auth). Routes: `/healthz` always 200, `/readyz` returns
  200 once cachestore self-test passed AND cluster has loaded its
  initial peer-set snapshot. Deployment template gains livenessProbe
  and readinessProbe entries.
- **C-3 (pipe-delimited metadata cache keys).** `metadata.mkKey`
  built `originID + "|" + bucket + "|" + key`. S3 object keys may
  legally contain `|`. Switched to length-prefixed encoding;
  in-memory only, no on-disk compatibility implication.
- **B-11 (refresh streak bumped on ctx-canceled).**
  `cluster.refresh` treated the `context.Canceled` from PeerSource
  during graceful shutdown as a discovery failure, bumping the
  streak counter and emitting a 'discovery failed' warning. Now
  short-circuits on ctx-canceled / ctx-deadline-exceeded.

### Smaller cleanups landed alongside

- **B-5**: `cachestore/s3.PutChunk` dropped the `&& size > 0`
  carve-out on the size validation.
- **B-8**: removed unreachable `len(peers)==0` branch in
  `cluster.Coordinator` (Peers() always returns >= 1 element).
- **B-10**: defensively clamp `end >= 0` in `chunk.IndexRange`.
- **Q-1**: extracted `fetch.lookupOrStat` helper shared by `GetChunk`
  and `FillForPeer` (was duplicated catalog/stat hot path).
- **Q-5**: removed unread `entry.at` field from `chunkcatalog`.
- **Q-7**: extracted `cleanupOnStartFailure` helper from `app.Start`
  (was duplicated three times for edge / internal / ops bind
  failures).
- **Q-8**: `app.Wait` loop-drains `errCh` on ctx-cancel rather than
  draining only one error.
- **Q-9**: introduced `unwrapAzcoreETag` helper in azureblob
  driver, replacing two open-coded `strings.Trim` sites.
- **S-1**: unexported `cluster.Resolver` -> `resolver` (no external
  consumer).
- **S-2**: `app.options.clusterOpts []cluster.Option` ->
  `clusterOpt cluster.Option` (was always 0 or 1 element).
- Doc comments added for the detached `runFill` context, the
  singleflight ctx-propagation tradeoff in
  `metadata.LookupOrFetch`, and the cluster-before-listener startup
  ordering.

### Deferred items (with rationale)

These findings were identified in the second pass but explicitly
deferred. Each has a reason documented here so they aren't silently
dropped from future remediation work.

- **Q-2 (8 MiB-per-fill peak heap, streaming validator).** Without
  the `fills_inflight` metric we chose to skip in this pass, we
  cannot measure actual incidence under load. Current behaviour is
  correct; the streaming-validator refactor touches the critical
  `runFill` path and risks subtle bugs in commit-after-serve.
  Revisit when metrics land and we observe real fill concurrency.

- **Q-3 (SHA-256 -> xxhash for rendezvous score).** Pure performance
  optimization. Today's load (small N peers, ~16 chunks/sec at
  1 Gbps, 5 peers = 80 hash/sec) makes SHA-256 a non-issue.
  Premature.

- **Q-4 (endianness consistency between chunk.Path LittleEndian and
  cluster.rendezvousScore BigEndian).** Cosmetic. Touching
  `chunk.Path` invalidates the on-store key (silent cache reset on
  first deploy after upgrade). Park alongside the next storage-key
  change.

- **Q-11 (multi-range request support).** Explicit MVP scope
  decision; documented in the edge handler. Multi-range returns 416
  today, technically RFC-non-compliant but the simplest
  reviewer-acceptable shape.

- **Q-12 (planRange helper for handleGet).** Worthwhile readability
  refactor but the handler has just-stabilised B-4 logic and is
  well-tested. Refactor risk > readability win.

- **S-3 (CoordinatorChecker interface for InternalHandler).** Tests
  currently construct a real Cluster with a single-self peer source
  and that suffices. Adding the interface expands surface area
  without immediate test pain.

- **S-4 (split SelfTestAtomicCommit into a separate interface).**
  Aesthetic only; current shape doesn't cause friction.

- **S-5 (split List out of origin.Origin).** Aesthetic. Would matter
  if we added a list-less driver, which isn't planned.

- **S-6 (TEST-ONLY listener-override options in a separate
  package).** Inline doc comments already mark them TEST-ONLY; no
  current cost.

- **O-1 (Prometheus metrics surface).** Explicitly deferred to a
  separate effort; the operator-observability tier wants more
  thought than the cleanup-pass shape supports.

### Verification

Every second-pass commit ran the full `make` chain (gofumpt,
golangci-lint, go test) plus `make orca-inttest`. For T2.1
(cachestore/s3 error mapping), inttest also served as the
verification gate that LocalStack 3.8 returns HTTP 412 on
If-None-Match conflict (rather than the legacy `InvalidArgument`
code we previously matched).

---

## Observability: structured debug logging

Before this pass orca had roughly 20 log call sites across the
codebase, all at Warn or Info level, and 5 of 8 packages had no
logger at all. Debug-level tracing was effectively impossible: the
boot-time level was hardcoded to LevelInfo, and there were no Debug
emissions to enable even if it weren't.

### What landed

- **`cfg.Logging.Level`** (commit "Add cfg.Logging.Level + ORCA_LOG_LEVEL
  override + AddSource"): YAML knob (`logging.level`) with values
  `debug` / `info` / `warn` / `error`. Default `info`. The
  `ORCA_LOG_LEVEL` environment variable overrides the YAML setting
  at process start; unknown values from either source surface as a
  parse error at config validation time. Uses `slog.LevelVar` so a
  future runtime-tunable path (signal- or endpoint-driven) can plug
  in without touching the handler.
- **`HandlerOptions.AddSource: true`** on the production JSON
  handler so every emission carries `source: {file, line, function}`.
  Replaces per-package `log.With("package", ...)` tagging; operators
  filter by source location instead.
- **`*slog.Logger` injection** into every previously logger-less
  package: `metadata`, `chunkcatalog`, `cluster` (via a `WithLogger`
  functional option), `cachestore/s3`, `origin/awss3`,
  `origin/azureblob`. All accept nil and fall back to
  `slog.Default()`.
- **Debug-level emissions** at every chunk-resolution decision point
  in `fetch.Coordinator`, every catalog operation in `chunkcatalog`,
  every cache-hit path in `metadata`, every backend operation in
  `cachestore/s3`, every origin call in `awss3` and `azureblob`,
  request entry/exit in `server` (both Edge and Internal), and
  per-cycle / per-transition emissions in `cluster.refresh`,
  `Coordinator`, and `FillFromPeer`.
- **`slog.LogAttrs` everywhere** (not the convenience form) so
  attribute evaluation is zero-cost when the configured level
  filters the emission out. Critical for the chunkcatalog.Lookup
  hot path where a single client request can trigger dozens of
  lookups.
- **Cross-package attribute taxonomy**: every chunk-related emission
  uses a `slog.Group("chunk", ...)` carrying `origin_id`, `bucket`,
  `key`, `index`. Operators can filter on `chunk.bucket=foo` across
  fetch, chunkcatalog, cachestore, and the server handlers with a
  single grep.
- **Existing Warn / Info callsites migrated to LogAttrs** alongside
  the new emissions so the codebase shares one consistent shape.
- **Sensitive-data audit**: account keys, access keys, signed URLs,
  and full ETags never appear in logs. The new `origin.ETagShort`
  helper truncates entity-tags to the first 8 characters at every
  call site where they are emitted. Object keys and bucket names
  are logged in full because they are part of the operator's
  diagnostic context.

### Operator workflow

```yaml
# configmap
logging:
  level: debug
```

or, without re-rendering the configmap:

```sh
kubectl set env deployment/orca ORCA_LOG_LEVEL=debug
kubectl rollout restart deployment/orca
```

Then filter the structured JSON output via, for example:

```sh
kubectl logs -l app=orca --tail=-1 | jq 'select(.chunk.bucket=="my-bucket")'
kubectl logs -l app=orca --tail=-1 | jq 'select(.source.file | endswith("fetch.go"))'
```

### Deferred (future work)

- **Per-request correlation IDs**: deliberately deferred. Threading
  a request-scoped logger through every fetch coordinator method
  requires ctx propagation work and touches many call sites. The
  shared `chunk` attribute group plus AddSource provides workable
  cross-request correlation in the meantime.
- **Prometheus metrics**: still deferred from the prior pass; debug
  tracing is the operator's diagnostic surface, metrics will arrive
  separately.
- **Runtime log-level switching**: the `slog.LevelVar` foundation is
  in place; a SIGUSR1 handler or `/loglevel` admin endpoint can
  plug in without touching the handler.
</content>
</invoke>