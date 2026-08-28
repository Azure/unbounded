# Gantry Fail-Open Response Header Deadline

Status: Implemented; live benchmark validation pending

## Problem Statement

Gantry is configured as containerd's first registry host, with the origin
registry as a fail-open fallback. During a cold pull, Gantry may discover peers
that have the requested digest but are all at their configured body-serving
limit. Those peers return HTTP 429 with `Retry-After`.

Before this change, Gantry retried those peers without committing an HTTP
response to the local containerd client. Containerd therefore saw no response
headers while Gantry resolved transient peer capacity pressure. On the measured
AKS containerd version, the request was canceled after 30 seconds waiting for
response headers. Containerd could then select the configured origin fallback
and download the layer directly from ACR.

This behavior converts transient peer saturation into origin payload traffic.
Increasing Gantry's peer retry count or retry duration does not prevent it,
because the 30-second cancellation is owned by containerd's HTTP client.

## Observed Behavior

Run `run-20260828-120256-1f1ce786` used 1,000 nodes and a 40 GiB image split
into 40 layers. Containerd was configured with six concurrent downloads and a
15-minute image pull progress timeout. Gantry used 20 peer attempts per discovery
round and a one-second peer rediscovery interval.

For node `aks-system-49682161-vmss000075` and layer
`sha256:062d3eeb86386998a1d82e1c5cebe7adc3257a2293dca3afd77230c115acafdc`, the
retained logs show:

1. At `12:11:55.177Z`, containerd sent a GET to local Gantry.
2. Gantry recorded 337 peer `busy` outcomes for this digest.
3. At `12:12:25.178Z`, containerd canceled the Gantry request with
   `timeout awaiting response headers`.
4. Gantry then recorded peer request errors caused by the canceled request
   context.
5. At `12:14:24.775Z`, containerd sent a GET for the same digest directly to
   ACR.
6. ACR returned HTTP 200 and the full layer body.

The final run measurements were:

- Gantry peer bytes served: `34,887,874,290,257`
- Gantry ACR Private Endpoint bytes: `9,082,525,142,732`
- ACR byte reduction versus the retained baseline: `80.75%`

A strict-routing control did not expose the ACR fallback host. The same class of
containerd retry therefore returned to Gantry instead of moving the layer to ACR.

## Previous Code Path

The peer transfer endpoint acquires a body-serving slot before opening and
serving a blob. If no slot is available, it returns HTTP 429 and
`Retry-After: 1`. HEAD requests are not subject to the body-serving limit.

The requesting Gantry classifies a peer 429 as capacity pressure and retries
providers with bounded jitter. In live stream-through mode, however, Gantry
writes the containerd-facing response headers only after a peer GET has returned
a body and Gantry has read enough bytes to inspect its prefix. An all-busy peer
set therefore leaves the outer request headerless.

Gantry already has a metadata-only origin path. `origin.Head` obtains the
content length and content type using request-scoped delegated authorization. It
does not transfer the blob body and does not increment origin body-pull counters.

## Implemented Solution

When a GET reaches a capacity-constrained peer result before Gantry has started
the containerd-facing response:

1. Issue a metadata-only HEAD to the configured origin for the requested digest.
2. Validate that the HEAD returned a non-negative content length and usable
   content type.
3. Write and flush the containerd-facing HTTP 200 response with:
   - `Docker-Content-Digest`
   - `Content-Length`
   - `Content-Type`
4. Continue the existing peer discovery and bounded-jitter retry loop.
5. Keep every peer 429 internal to Gantry.
6. If a peer returns 200 but does not produce its first body byte within the
   configured rediscovery interval plus bounded jitter, close that response and
   continue with the next peer from the same verified offset.
7. When a peer supplies content, require its reported total size to equal the
   HEAD metadata size and append its body to the already-open response.
8. If a peer fails after partial delivery, request the next peer with a Range
   beginning at the verified byte offset and continue the same outer response.
9. Complete only after the expected byte count and digest are verified.

If a peer returns 200 before capacity pressure is observed, Gantry uses that
peer response's size and content type to flush the same outer headers
immediately. It does not wait for a body prefix. The bounded first-byte rule
still applies, so an accepted peer that produces no body does not bind the
outer response to that peer.

The origin HEAD and peer body selection are independent. Sending the outer 200
does not bind Gantry to a particular peer. A peer GET that returns 200 supplies a
candidate body reader. Producing the first body byte selects that candidate for
streaming; header-only candidates are closed after the bounded first-byte window
so Gantry can continue rotating peers.

The proposed capacity path never issues an origin GET. Its origin traffic is
limited to authentication and metadata-only HEAD requests.

## Failure Semantics

### Before Outer Headers

If the origin HEAD fails, Gantry has not committed an outer status. Gantry can
return the corresponding error status, preserving containerd fail-open behavior
for a genuine origin, authorization, or local Gantry failure.

### After Outer Headers, Before Body Bytes

Gantry continues peer retries until one of the following occurs:

- a peer or local cache supplies the digest;
- the containerd request context is canceled;
- a hard Gantry or DHT failure occurs.

Gantry cannot replace the committed 200 with a later 429 or 503. On cancellation
or hard failure it terminates the incomplete response without writing an HTTP
error body into the blob stream. Containerd must not commit incomplete content.
A subsequent containerd retry may use the configured origin fallback.

### After Partial Body Delivery

Gantry resumes from another peer at the exact verified offset. Every resumed
peer must report the same total content size. If no source can continue, Gantry
terminates the incomplete response. The final digest verifier remains the
integrity boundary.

## Timeout Behavior

Flushing the outer 200 removes the observed 30-second response-header failure
for capacity-constrained requests. While no body bytes are available, the
containerd pull is instead subject to its body-progress policy. The benchmark
configures `image_pull_progress_timeout = "15m"`.

This proposal does not claim that 15 minutes is correct for every deployment.
The important change is that transient peer saturation is governed by the
operator-configurable body-progress policy rather than an earlier header
silence deadline.

## Metadata Handling

The first implementation performs an origin HEAD on the first peer 429, before
consulting cold-start. It does not add a cross-request metadata cache. This
keeps authorization and invalidation semantics unchanged while ensuring the
cold-start polling budget cannot consume the outer response-header deadline.

A future optimization may retain successful metadata from containerd's preceding
HEAD request. Any such cache must be bounded and preserve authorization scope;
it must not expose the existence or metadata of a digest to a request with a
different authorization context.

## Invariants

- Peer 429 is never forwarded to containerd.
- Capacity pressure never initiates an origin body GET inside Gantry.
- Outer headers are written at most once.
- No HTTP error body is appended after outer 200 has been committed.
- Every peer body must match the metadata size.
- Peer switches resume at the verified offset.
- Containerd receives exactly the declared content length on success.
- The completed body must match the requested digest.
- Hard failures before outer headers retain fail-open behavior.

## Validation Plan

Unit tests must establish:

1. Repeated peer 429s followed by a peer hit produce one origin HEAD, zero origin
   GETs, an early outer 200, and the correct body.
2. The outer headers become observable before the peer becomes available.
3. The retry loop remains active after outer headers are flushed.
4. A peer that returns 200 without body bytes is closed and replaced by another
   peer.
5. The same rotation works when the bodyless 200 is the first peer response and
   no origin HEAD has primed the response.
6. A peer-reported size mismatch terminates the response.
7. A partial peer stream followed by a busy alternate keeps retrying from the
   exact offset.
8. A hard failure after outer 200 does not append an HTTP error body.
9. An origin HEAD failure or unusable content type before outer headers
   preserves an error status.

A live fail-open benchmark must then verify:

- 1,000/1,000 workload completion;
- no response-header timeout events for Gantry URLs;
- ACR Private Endpoint bytes remain close to Gantry-attributed origin bytes;
- peer 429s remain visible while direct ACR layer GETs do not increase because
  of those 429s;
- the monitor reports Gantry-path coverage separately from total pull progress.

## Alternatives Rejected

### Return Peer 429 to Containerd

A final 429 marks Gantry as failed and permits containerd to select the next
registry host. It therefore accelerates origin fallback rather than preventing
it.

### Start a Gantry Origin GET

Starting an additional origin body stream while designated pullers are already
downloading the same digest duplicates payload traffic. The proposed solution
uses only origin HEAD metadata.

### Increase the Busy Retry Duration

A longer Gantry retry duration does not help while the outer request remains
headerless; containerd cancels first.

### Require Strict Containerd Routing

Strict routing prevents origin bypass but removes production fail-open behavior.
The solution must work while the origin remains configured as a fallback for
hard failures.
