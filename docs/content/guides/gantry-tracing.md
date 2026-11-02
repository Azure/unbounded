---
title: "Trace Gantry Pulls"
weight: 12
description: "Follow an image pull end to end across kubelet, containerd, Gantry, peers, and the upstream registry."
---

Gantry instruments its pull path with OpenTelemetry. Because containerd 2.x
already traces image pulls and propagates the W3C `traceparent` header on its
registry calls, Gantry joins the trace containerd started instead of ending it.
One trace then shows where cold-start time actually went.

## What the Trace Shows

A pod start produces a single trace:

```text
kubelet  PullImage
  containerd  PullImage
    containerd  fetchLayer
    Gantry      gantry.mirror.serve_digest
      Gantry    gantry.peer.fetch          # peer path
        Gantry  gantry.transfer.serve      # on the seeding node
      Gantry    gantry.origin GET          # upstream path
```

When a layer is already cached the trace stops at the mirror span, and the
`gantry.source` attribute reads `local`.

## Turn It On

Tracing is off by default. Nothing is exported and no collector connection is
opened unless `OTEL_TRACES_EXPORTER` selects OTLP.

Set the standard environment variables on the Gantry agent:

```yaml
env:
  - name: OTEL_TRACES_EXPORTER
    value: otlp
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "http://otel-collector.observability:4317"
  - name: OTEL_SERVICE_NAME
    value: gantry
```

| Variable | Effect |
| --- | --- |
| `OTEL_TRACES_EXPORTER` | Must be `otlp`. Any other value, including `otlp-http`, leaves tracing off. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/gRPC collector endpoint. |
| `OTEL_EXPORTER_OTLP_HEADERS` | Auth headers for a managed collector. |
| `OTEL_SERVICE_NAME` | Service name in the collector's service map. Defaults to `gantry`. |
| `OTEL_RESOURCE_ATTRIBUTES` | Additional resource attributes; `service.name` here is used when `OTEL_SERVICE_NAME` is unset. |
| `OTEL_BSP_SCHEDULE_DELAY` | Batch interval in milliseconds. Defaults to 5000. |

On a Helm install, pass the variables through the agent pod template env. At
startup a tracing agent logs `tracing enabled`.

An unreachable collector degrades to an observability outage, not a data-plane
outage: the agent logs a warning at startup and serves pulls normally.

## Span Attributes

| Attribute | Meaning |
| --- | --- |
| `gantry.digest` | Digest being served. |
| `gantry.kind` | `layer` or `manifest`. |
| `gantry.media_type` | OCI media type of the content. |
| `gantry.bytes` | Bytes delivered by the span, summed across every provider attempt. |
| `gantry.cache.result` | `hit` or `miss` for the local store lookup. |
| `gantry.source` | `local`, `peer`, or `upstream`. |
| `gantry.peer.id` | Peer that served or was attempted. |
| `gantry.registry` | Resolved upstream registry. |
| `gantry.repository` | OCI repository in the request path. |

`gantry.source` on the mirror span is the quickest answer to "why was this pull
slow": `local` means it never left the node, `peer` means it crossed one
network hop inside the cluster, and `upstream` means it went to the registry.

The span reports `local|peer|upstream` while the
`gantry_mirror_bytes_served_total{source}` metric reports
`cache|peer|origin`. The metric spelling is an existing dashboard contract and
did not change; the trace spelling is chosen to read unambiguously next to
`gantry.registry` on the same span.

`gantry.bytes` on the mirror span is a per-request total, not the size of the
last chunk. A live stream that resumes, where one peer stalls partway and
another serves the remainder, reports the whole delivery. The counter keeps its
per-chunk increments so cluster totals per source stay correct.

## Signed URLs

Blob fetches follow registry redirects to pre-signed storage URLs, and those
URLs carry the signature in the query string: `sv`, `sig`, and `se` for Azure
Blob Storage, `X-Amz-*` for S3, `X-Goog-*` for Google Cloud Storage.

Gantry replaces those values with `REDACTED` in the `url.full` attribute before
the span is exported. Out of the box OpenTelemetry strips only user information
from the URL and records the query verbatim, so without this the pre-signed
credential would sit in your trace backend, readable by anyone with dashboard
access and replayable until the signature expires.

Ordinary query parameters are left alone, so a `tags/list` cursor or a blob
`partNumber` still shows up. Short signed-parameter names are matched only when
the whole parameter name matches, because `st` and `se` occur inside ordinary
words like `last` and `statement`, and redacting those would make a trace
misreport a request that carried no credential.

Only the recorded attribute changes. The request that goes out on the wire keeps
the original URL, so the storage provider validates a real signature and the pull
succeeds. This holds with tracing turned off as well; not enabling OTLP is not a
reason for a pull to behave differently.

Gantry sets `gantry.url.redacted` to `true` when it replaced a value and `false`
when the request had nothing sensitive. The flag exists so a matcher that stops
matching is visible in the data rather than quietly exporting signatures behind a
URL that still looks redacted.

## Health Checks

`/healthz` is excluded from tracing entirely rather than given its own span
name. A request that arrives with no `traceparent` is always sampled under the
parent-based policy, so a named probe span would still be exported on every
probe interval, which is collector traffic unrelated to pulls.

## Sampling

Gantry uses a parent-based sampler, so it honors the sampling decision
containerd already made. If containerd did not sample a pull, Gantry does not
restart the trace. A request that arrives with no `traceparent` at all is
sampled, which keeps the agent traceable on its own.

The propagator stays active even when the exporter is off, so Gantry still
forwards an existing trace rather than breaking it.

## Both Backends

Tracing initializes on the common agent startup path, before the Racer and
classic-containerd backends are selected. Both backends build a mirror server
and an origin client, so initializing per-backend would leave one of them
untraced and without the W3C propagator, unable to forward trace context even
when a trace was already in flight.

## Reading a Cold Start

For a multi-layer image pulled onto an empty node, look for:

- Long `gantry.origin GET` spans under `gantry.mirror.serve_digest`. The
  first node to pull an image pays this cost for every layer.
- `gantry.peer.fetch` spans with a `gantry.transfer.serve` child on another
  host. That is the swarm serving the layer instead of the registry.
- A `gantry.mirror.serve_digest` span with no children and
  `gantry.source=local`. The cache served it.

If a whole rollout shows `gantry.source=upstream` on every node, the cache is
not seeding. Check `p2p_dht_health_score` and the cold-start path rather than
the registry.
