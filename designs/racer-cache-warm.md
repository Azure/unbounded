# Integrated Racer warm

## Contract

Warm is a HEAD retention request, not a second fetch protocol. The SDK exposes
`Client.Warm(ctx context.Context, request Request, ttl time.Duration, opts WarmOptions) (WarmResult, error)`.
It rounds a positive TTL up to whole seconds and sends `Cache-Control: max-age`.
`WarmOptions.ETag` optionally pins a strong ETag. Ordinary `Stat` does not warm.

The dataplane resolves metadata first, then admits one exact cache/key/ETag.
It clamps the TTL to `RACER_WARM_MAX_TTL_SECS` (default 3600). The TTL starts at
HEAD admission and runs during filling. A successful HEAD returns metadata and,
only when admitted, `Racer-Retain-Until`: an absolute Unix timestamp in milliseconds.
This means admission, not completed fill. A `200` without the header is a retention
refusal. A zero-length object needs no page reservation.

Repeated requests share the exact-version job and can extend its deadline without
shortening an existing hold. The registry is node-wide; completed jobs remain
tracked until expiry. Defaults are 16 tracked versions, a two-page fill window,
and a 600-second independent fill timeout. Configuration validates both seconds
limits in 1..86400, jobs in 1..1024, window in 1..64, and retained percentage in
1..100 (default 50).

Sources: `pkg/racersdk/client.go:215-247`, `pkg/racersdk/types.go:120-133`,
`cmd/racer-dataplane/src/read.rs:177-240`, `cmd/racer-dataplane/src/read/warm.rs:22-47,163-215,395-511`,
`cmd/racer-dataplane/src/config.rs:188-194`.

## Fill and foreground reads

Admission reserves local worker capacity before returning the retain header.
The job owns its own scope, so returning or disconnecting HEAD does not end an
admitted fill. It owns a separately charged, zeroizing credential context in
memory. Credentials and jobs never enter checkpoint state. The fill timeout and
retention expiry stop new work; accepted I/O still requires completion cleanup.

Pages start in ascending order through a bounded pipeline. Normal acquisition
still selects local copies, peers, owners, and origin routes. Warm does not change
placement or topology, reserve remote holds, or fetch directly from an origin
that normal routing would not use. Existing verified local disk copies are reused.

Ordered and unordered local reads join the exact-version job. They wait for its
disk-publication frontier even for pages outside the current window. There are no
priority jumps or competing foreground fetches. Reads stay pipelined and can
consume published pages before the whole object completes. Reader credits,
deadlines, and waiter limits still apply. When a job fails or expires, waiters can
fall back to normal fill. A missing held copy while the job is active is an error,
not permission to race it.

Warm does not extend current-version metadata freshness. Unpinned reads still
refresh expired metadata and may select another version. Callers should pin the
later `Get` to `WarmResult.Metadata.ETag` to consume the warmed version. A different
ETag does not join or inherit the old job.

Sources: `cmd/racer-dataplane/src/security.rs:653-657,736-764`,
`cmd/racer-dataplane/src/read/warm.rs:268-287,339-368,684-740`,
`cmd/racer-dataplane/src/read/dispatch.rs:404-510,888-909,979-995`,
`cmd/racer-dataplane/src/read/metadata.rs:761-808`.
The ordered/unordered test asserts pending reads create no Fill driver or outbound
acquisition: `cmd/racer-dataplane/src/read/dispatch.rs:1163-1235`.

## Conservative storage reservation

The implemented policy charges **one whole segment per reserved page**. It does
not use a dedicated warm tail. It does not assume pages pack together, even when
they are short or already cached. Existing copies keep their original locations;
warming does not rewrite them to improve packing.

This conservative capacity charge supports a hard eviction veto under
fragmentation: any segment containing a held record must stay. Each worker limits
total reserved pages by the smaller of its retained percentage of segment count,
its segment count minus the free reserve, and its retained percentage of page-index
capacity. Unused page-index reservations also count before fetching begins.
Admission can therefore fail even when the payload bytes would fit, or because
one worker fills before another. Increasing the percentage does not remove the
free-segment reserve.

Hard holds bypass second-sight and block both page-index eviction and segment
reclamation. Progress requires completed disk publication of verified ciphertext,
not queue acceptance. If a later page fails, completed disk pages retain their
deadlines. Once in-flight persistence finishes, cleanup releases only unused
reservation capacity. Retention does not override corruption, missing keys,
cache removal, or failed storage.

Sources: `cmd/racer-dataplane/src/store/holds.rs:1-3,16-33,64-71`,
`cmd/racer-dataplane/src/store/catalog.rs:174-259,285-313`.
Tests assert hard eviction refusal and partial-failure preservation at
`cmd/racer-dataplane/src/store/catalog.rs:1155-1194,1216-1233` and reuse without
rewriting at `cmd/racer-dataplane/src/store/tests.rs:327-354`.

## Restart and compatibility

Checkpoint v4 adds completed-record hold deadlines. Older checkpoint versions are
discarded; with no compatible candidate, the node starts cold, including in file
mode. Do not reset cluster or node identity to upgrade disposable cache storage.

Only unexpired completed disk holds can recover. Background jobs, waiters,
credentials, and unused reservations do not recover. Live hold expiry uses both
wall and monotonic time; after restart, only the saved wall deadline is available.
Payload writes and checkpoint publication do not add `fsync`. A process or host
crash can lose pages or retention state; this is not durable storage or a guarantee
that an admitted object will finish loading.

Sources: `cmd/racer-dataplane/src/store/checkpoint.rs:418-432`,
`cmd/racer-dataplane/src/store/holds.rs:1-3,24-28`,
`cmd/racer-dataplane/src/store/tests.rs:393-421`.

## Deployment and observability

The deployment ConfigMap exposes the five validated settings:
`RACER_WARM_MAX_TTL_SECS`, `RACER_WARM_FILL_TIMEOUT_SECS`, `RACER_WARM_MAX_JOBS`,
`RACER_WARM_WINDOW_PAGES`, and `RACER_RETAINED_SEGMENT_PERCENT`.
Keep their defaults aligned with `config.rs` and the public reference. Existing
administrator ConfigMap data is preserved; rebuild the operator for embedded
template changes and regenerate standalone manifests with `make racer-manifests`.

Do not treat an accepted HEAD or writer enqueue as completion. Existing
`racer_disk_publications_total` counts disk publications, not whole-object warm
completion. Any warm-specific metric contract must follow the implemented
telemetry; this document does not assign names to planned metrics.

Source: `cmd/racer-dataplane/src/telemetry.rs:2494`.
