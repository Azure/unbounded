# Independent client/origin v1 conformance

## Authority and scope

Required contract: `/home/azureuser/code/unbounded/cmd/racer-dataplane/CLIENT_ORIGIN_API.md`,
read completely (lines 1-176). The current SDK is `/home/azureuser/code/unbounded/pkg/racersdk`.
Rust paths below are relative to `cmd/racer-dataplane` in the implementation worktree.
The contract's historical `Unimplemented` statements describe its baseline, not
required behavior. No production files belong to this conformance work.

The owned integration target uses real Unix stream sockets and production
`HttpIo`, `RequestParser`, origin validators, and `Responses`. Socket-pair tests
exercise framing and parser boundaries but do not exercise `ClientListeners` or
the complete `Coordinator`. The Go fixture is compiled against the actual SDK by
a Go overlay that adds one test file; it does not copy or modify SDK source.

## Confirmed owner handoffs

1. **High confidence: raw head size is recounted after normalization.** Contract
   lines 44,47 requires the actual entire wire head to fit 32768 bytes. A 32768-byte
   HEAD with `X:value` is accepted by the codec but rejected by `RequestParser`:
   `src/client/request.rs:89-102` adds four bytes per field even when the optional
   SP was absent. `raw_uds_head_limit_does_not_invent_separator_bytes_for_unknown_fields`
   fails. Client/HTTP owners: carry the validated raw head length in a received-head
   envelope or remove the reconstructed wire-size check at the raw-validated entry
   point. Keep an explicit bound for manually constructed heads if needed.

2. **High confidence against the required contract: expiry whitespace is accepted.**
   Contract lines 69,72-73 explicitly prohibits whitespace in canonical numbers.
   `src/origin/protocol.rs:20-29,32-39` trims it before parsing expiry. The existing
   `src/origin/metadata.rs:110-116` test explicitly accepts it. The Go SDK also
   trims fields at `pkg/racersdk/wire_conn.go:137-145`, before decimal validation
   in `pkg/racersdk/wire.go:409-414`. Thus agreement between implementations alone
   would miss this discrepancy. `raw_uds_origin_expiry_rejects_noncanonical_whitespace`
   fails. Origin/HTTP and SDK owners: preserve canonical numeric field bytes
   after the header separator and validate before OWS normalization. This does
   not change the opaque-field rule, which is already separately raw-validated.

3. **Resolved during concurrent owner work: recoverable parser errors.** The
   initial review found `receive_head_limited(...).await?` closing malformed
   requests before the empty-error writer. The current
   `src/http/io.rs:174-185,213-230` introduces `receive_request_head_limited`, whose
   inner error retains a fenced, poisoned lease. The client adopts it at
   `src/client/listener.rs:309-325`. The independent
   `raw_uds_parser_errors_retain_a_writable_lease_for_empty_errors` now exercises
   that public API and the real error writer for duplicate framing, transfer
   coding, opaque whitespace, and oversized heads. It permits transport reset
   after received error bytes because closing without draining is required;
   it still requires one complete, empty 400/431 response. Full listener binding
   remains outside this integration target.

## Checks and coverage

Initial command: `cargo test --test client_origin_conformance -- --nocapture`.
Result: 10 passed, 2 failed as above, 1 opt-in SDK test ignored. A prior
`cargo test --lib --no-run` encountered concurrent `app.rs` test construction errors;
the integration target subsequently built successfully without editing those files.

Final all-inclusive run: **17 passed, 2 failed, 0 ignored**. Both failures are the
unmodified contract regressions in handoffs 1 and 2. The built SDK test passed:
three client sizes (0, 3, 50331661), followed by nineteen raw origin cases. The
recoverable parser-error regression passed after the owners' production change.
Scoped `rustfmt --check`, `gofumpt -d`, and `git diff --check` passed.

A scoped `make fmt GO_PACKAGE_DIRS=cmd/racer-dataplane/tests/conformance
GO_PACKAGE_PATTERNS=./cmd/racer-dataplane/tests/conformance/...` initially ran
gofumpt but its lint step could not typecheck the package-private SDK fixture as a
standalone package. The fixture now has a `.go.txt` suffix, is formatted directly,
and successfully compiles in its intended SDK overlay. No repository-wide Go
lint success is claimed. Intermediate telemetry compilation errors from concurrent
owner changes also cleared before the final run.

## Coverage against every contract section

Contract line ranges in this table refer to the required document above.
"Source" is an audit observation, not end-to-end proof.

| Contract | Independent evidence and remaining gap |
| --- | --- |
| 14-18 UDS, canonical paths, endpoint ownership | Socket pairs plus pathname Go/Rust UDS interoperability; DNS/path boundary tests. `control/caches.rs:55-95` validates canonical paths. Full application cache lifecycle is not exercised. |
| 19-28 exact target, methods, Host, forbidden transports | Raw invalid query/fragment/percent/uppercase/absolute/extra-slash targets; HEAD and GET; POST 405; HTTP/1.0 and upgrades rejected. No proxy/TLS/compression appears in SDK client transport (`pkg/racersdk/client.go:77-85`). No independent HTTP/2 preface test. |
| 29-36 bodyless requests, fixed framing | Raw zero/nonzero/list lengths, transfer/content identity coding, Expect/Trailer; real fixed-body reads with CRLF and HTTP-looking payload; truncation rejected. Response writer fields checked on the wire. |
| 40-45 repeated singleton/unsupported conditions | Every named singleton duplicated case-insensitively in both directions. Response duplicate tests establish a valid nonduplicate control first. Folded lines and all listed conditional fields rejected. |
| 47-55 aggregate/opaque limits and exact bytes | 32767/32768/32769 request heads; 32768/32769 response heads; 1/8192/8193 opaque values; exact separator SP, edge whitespace, NUL/DEL/tab, every byte 0x80-0xff; one aggregate-count discrepancy remains. |
| 57-62 context forwarding and lifetime | Go client continuation preserves both fields and key through Rust parser; Go origin callback verifies them. Sequential Rust requests do not inherit fields. Source zeroization: `model/context.rs:23-54`, `http/codec.rs:21-24`, `http/io.rs:28-31,239-241,297-301`; pooled Idle holds only FD/quota/time (`http/pool.rs:29-33`). No unsafe freed-memory inspection, allocator-level zeroization proof, or full log/cache/durable-retry audit. |
| 68 strong ETag | Empty tag, literal comma/backslash, weak/list/wildcard/space/embedded-quote rejection. 8192-byte ETag boundary is not independently exercised. |
| 69-74 canonical metadata and immutable size | MaxInt64 size/expiry, overflow/sign/padding rejection; remaining expiry-whitespace failure. SDK fixture checks initial metadata remains unchanged when continuation expiry refreshes. Origin page wrong tag/bounds rejected. Full same-tag changed-size cache publication is not independently exercised. |
| 75-79 freshness and zero-TTL cohort | Source removes completed cohort from active table before waking (`read/metadata.rs:174-185`), freshness lookup uses `current.resolve` (`read/metadata.rs:607-624`). Existing cohort assertions at 835-863 check new registration becomes leader despite live old waiters. SDK wire run continues after expiry zero. A concurrent real-socket cohort/coordinator test is still required. |
| 83-98 HEAD/bootstrap/client ranges | Pinned HEAD parse and response; empty bootstrap 200 without range; valid nonempty 206; closed/open/suffix normalization, zero suffix and empty object unsatisfiable, malformed/reversed/overflow rejection. Go client sends exactly one full-remainder pinned request for a 3P+13 object, verified byte-by-byte with 32 KiB staging. This scripted peer proves SDK/Rust HTTP/parser interoperability, not production page acquisition. |
| 100-110 whole-page origin | Built Go origin accepts nominal/short-final page and rejects partial/open/suffix/unaligned/cross-page requests; aligned out-of-object page gives 416. Rust page validator accepts exact final page and rejects shifted/short/tag-mismatched responses. Nominal MaxInt64 end arithmetic source: `origin/client.rs:244-252`; P is a power of two dividing MaxInt64+1, so the last representable page nominal end is exactly MaxInt64. No enormous body allocation is needed or attempted. |
| 112-115 missing/pin/credentials/selected version first | Built SDK origin exercises fresh 404, pinned 412, 401,403,503 and mismatched successful pin 502. Client error writer verifies selected-size 416 field. Coordinator source resolves pinned metadata before range (`read/serve.rs:210-223`); no full old-pin/deleted-object cache test yet. |
| 119-137 error status/framing/no retries | All documented statuses checked, empty bodies/no validators/expiry, 405 Allow and 416 selected size. Redirect/unknown/informational responses rejected. Oversized origin head is classified by raw layer; owner OriginClient maps receive errors to BadGateway (`origin/client.rs:393-399`). No independent real OriginClient retry/cancellation exercise in this target; existing owner UDS tests cover its calls. |
| 139-143 validation before success and late abort | Real Rust response validator, fixed-body truncation, and RangeStream owner-acquisition failure after a committed 206; exactly one status and no appended error body. Go callback short/overlong bodies abort before completing advertised bytes. |
| 145-154 callback ownership/EOF probe | Built SDK origin exercises nil HEAD, nil empty bootstrap, short and overlong GET. Source closes returned body even with callback error (`pkg/racersdk/origin.go:265-290`) and probes before final write (425-450). Exact-once close, unexpected HEAD body, empty non-EOF body, final-bytes non-EOF error, probe timeout, and cancellation are not independently exercised here. |
| 156-163 detection limits | Tests assert framed bytes only and make no claim that HTTP can detect unobserved extra bytes or wrong content with the right ETag. |
| 167-176 raw guard/read-ahead/parser errors | Actual Unix peers bypass normalization. Delimiter-like body bytes are preserved; sequential requests and writable malformed-head errors are checked. No page-sized scratch is allocated by the cross-language client fixture. |

## Reproduction

From `cmd/racer-dataplane`:

```sh
cargo test --test client_origin_conformance -- --nocapture
RACER_SDK_ROOT=/home/azureuser/code/unbounded cargo test --test client_origin_conformance -- --include-ignored --nocapture
rustfmt --edition 2024 --check tests/client_origin_conformance.rs tests/conformance/sdk.rs
gofumpt -d tests/conformance/sdk_fixture_test.go.txt
```

The `.go.txt` fixture is deliberately not a standalone Go package. Its overlay
adds it to the SDK package to use only private socket-path seams; the build is
`go test -overlay <generated overlay> -c -o <fixture binary> ./pkg/racersdk`.
All temporary outputs stay under project directories and are removed by guards.
The SDK client uses a proc-fd socket alias to fit deep worktree paths. SDK origin
uses a real directory under `$RACER_SDK_ROOT/tmp` because its production path
validator correctly rejects proc-fd symlink ancestors.
