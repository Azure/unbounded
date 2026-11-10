# Gantry streaming cold-start follow-ups

Status: deferred. Recorded after the v0.23.0 mixed regular/streaming image smoke
test on November 9, 2026. Neither item is implemented by this document.

## TODO: Authenticate private-ACR origin range fallback

The streaming handler falls back to origin when local content, cached ranges,
and advertised peers cannot satisfy a read
(`internal/gantry/streaming/server.go:191-299`). The snapshotter supplies a
canonical ACR blob URL, but the origin range client does not perform registry
authentication (`internal/gantry/overlaybd/block_mount_linux.go:205-209`,
`internal/gantry/streaming/origin.go:108-115`).

Observed evidence:

- All 20 smoke-test Pods eventually became Ready across ten workers.
- Three streaming Pods initially failed container creation while containerd
  accessed `/etc/passwd`. The kernel reported an NBD read error, and containerd
  reported `input/output error`. Kubelet retried successfully; these were not
  running-container restarts.
- Each affected worker recorded one streaming-origin error.
- A controlled request through an affected worker's Gantry endpoint for an
  uncached, existing private-ACR blob returned HTTP 401,
  `origin authorization failed`.
- An authenticated request for the same blob and byte range returned HTTP 206
  and all 4,096 requested bytes. The original three requests' upstream HTTP
  statuses were not logged.

Follow-up:

- Support authenticated resolution/fetching for canonical private-registry blob
  URLs using the appropriate existing authentication mechanisms.
- Preserve signed-URL handling without attaching registry credentials to signed
  data URLs or forwarding credentials to peers.
- Validate a genuinely cold private-ACR range read without relying on background
  prefetch or already available peer content.
- Preserve enough redacted diagnostics to identify origin status and the failed
  range without exposing authorization values or signed URL queries.

The absence of added registry credentials is intentional for signed data URLs
(`designs/gantry-acr-artifact-streaming-implementation-plan.md:298`), but does not
provide authentication for canonical private-ACR URLs.

## TODO: Add bounded chair/peer waiting before direct-origin fallback

The expected follow-up flow is:

```text
local content / range cache
  -> advertised peers
  -> designated chair cold-pull coordination and bounded peer rediscovery
  -> authenticated origin fallback after the wait budget expires
```

Currently, the range handler queries advertised peers and then goes directly to
origin (`internal/gantry/streaming/server.go:227-299`). Chair-driven complete-layer
pulls are dispatched separately by background reconciliation
(`cmd/gantry/streaming_layer_reconciler.go:70-127`).

This would intentionally change the existing streaming plan, which says not to
wait for an in-progress chair pull before using signed origin
(`designs/gantry-acr-artifact-streaming-implementation-plan.md:45-46`). It is not
merely an authentication fix.

Follow-up:

- Reuse existing cold-start coordination to confirm or trigger the designated
  chair's pull, rather than starting redundant per-node origin downloads.
- Retry peer discovery and exact-range availability within a bounded wait.
- Respect request cancellation and propagate failures explicitly.
- Fall back to authenticated origin when the wait budget is exhausted; never
  wait indefinitely for an unavailable chair or missing layer.
- Validate delayed provider availability, concurrent requests, chair failure,
  cancellation, and timeout behavior.

Choose the wait budget and its relationship to filesystem-read timeouts during
implementation. Waiting does not eliminate the need for working authenticated
origin fallback.

## Smoke-test artifacts

The local run recorded creation-to-container-start times of 6.8 seconds on
average for regular images and 5.3 seconds for streaming images. These are one
mixed-pull smoke run, not a controlled benchmark; caches were not cleared.

Per-Pod timings, events, and RCA evidence are under
`tmp/gantry-smoke-startup/` in the originating workspace. This directory is
gitignored and is not included with this document.
