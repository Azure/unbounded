---
title: "Trace Gantry Pulls"
weight: 12
description: "Enable OpenTelemetry tracing for Gantry and use spans, attributes, logs, and metrics to follow an image pull and diagnose slow transfers."
---

An image pull through Gantry crosses several processes. The kubelet asks containerd for an image, containerd asks the node-local Gantry mirror, and the mirror either reads the local content store, streams the digest from a peer, or fetches it from the upstream registry. Tracing connects those hops into a single request so you can see which source served every layer, how many bytes each source delivered, and where a slow pull spent its time.

Gantry instruments the mirror, peer transfer, and origin client paths with OpenTelemetry. It does not create its own trace format and it does not require a proprietary backend. If your cluster already has an OTLP collector, you enable tracing with environment variables on the agent and read the result in whatever trace viewer you use for the rest of the platform.

The feature is off by default. A default agent exports nothing and opens no collector connection, and every instrumentation point resolves to a non-recording span. That keeps the default DaemonSet free of collection cost and keeps a misconfigured collector from affecting image pulls.

## What the Trace Shows

The useful mental model is one trace per pull request, with one mirror span per digest. Containerd's registry client already speaks the W3C TraceContext format and sends a `traceparent` header on its registry requests. Gantry extracts that header on the mirror endpoint and continues the trace instead of starting a new one. The mirror span becomes a child of containerd's pull span, and every peer or origin request the mirror makes becomes a child of the mirror span.

When the whole path is instrumented, a trace reads from the outside in:

```text
PullImage                      kubelet or containerd
└─ gantry.mirror.serve_digest   Gantry mirror on the requesting node
   ├─ gantry.peer.fetch        Gantry transfer client, when a peer serves
   │  └─ gantry.transfer.serve Gantry transfer server on the seeding node
   └─ gantry.origin GET        Gantry origin client, when the registry serves
```

The span names are deliberately stable and do not contain digests. A layer pulls through the same `gantry.mirror.serve_digest` name whether it is 2 MB or 2 GB, so a trace viewer can group every layer of an image into one row and a query can select all mirror work without matching a URL pattern. The digest is an attribute, not part of the name.

The hierarchy answers the first question in any slow-pull investigation: which hop is slow? A long mirror span with a short child span means the time went somewhere else, usually the local content store or the response body copy. A long peer fetch with a long transfer serve child means the seeding node was slow to read or send. A long origin span means the registry or the network path to it is the bottleneck.

A trace also shows fanout. A cold node that has just discovered a swarm may produce one peer fetch per layer, each landing on a different seeding node. A warm node that already holds the image produces one short local span per digest and no children at all. Comparing those shapes across nodes is often enough to explain why one node pulls an image in seconds and another takes minutes.

Tracing does not cover everything. It instruments the Gantry mirror handler, the Gantry peer transfer client and server, and the Gantry origin HTTP client. It does not instrument the chair RPC, libp2p discovery, containerd's content store, or the container runtime's unpack step. Containerd's own spans appear only when containerd is configured to export traces, and kubelet spans appear only when kubelet tracing is enabled. If containerd sends no `traceparent`, the mirror span is a root span and is still sampled.

## Turn It On

Tracing has no command-line flag. It is controlled entirely by environment variables on the agent container, which means the same mechanism works for a Helm release, a raw DaemonSet, or a local test process. The only variable that turns collection on is `OTEL_TRACES_EXPORTER`.

Gantry accepts one exporter value, `otlp`. The comparison is case-insensitive and surrounding whitespace is ignored, so `OTLP` and ` otlp ` both work. The value `otlp-http` is deliberately rejected, because Gantry only implements OTLP/gRPC. Any other value, including `none`, leaves tracing disabled. A misspelled exporter does not fall back to HTTP; it simply exports nothing, which is why the startup log matters.

### Environment variables

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `OTEL_TRACES_EXPORTER` | Yes, to enable | unset | Must be `otlp`. Any other value leaves tracing off. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Recommended | `https://localhost:4317` | OTLP/gRPC target, including the `http` or `https` scheme. |
| `OTEL_EXPORTER_OTLP_HEADERS` | No | none | Comma-separated `key=value` metadata for a managed collector. |
| `OTEL_SERVICE_NAME` | No | `gantry` | Service name reported to the collector's service map. |
| `OTEL_RESOURCE_ATTRIBUTES` | No | none | Additional resource attributes in comma-separated `key=value` form. |
| `OTEL_BSP_SCHEDULE_DELAY` | No | `5000` | Batch span processor delay in milliseconds. |

The endpoint, headers, TLS settings, compression, and timeout are read by the OTLP/gRPC exporter itself, so any other standard `OTEL_EXPORTER_OTLP_*` variable you already use for other services applies here too. Gantry does not parse those values and does not rewrite them.

The service name resolves in a fixed order. `OTEL_SERVICE_NAME` wins, then a `service.name` entry inside `OTEL_RESOURCE_ATTRIBUTES`, then the literal `gantry`. The resolved name is what the agent logs at startup and what a collector uses to group spans in its service map. If you run more than one Gantry deployment in a cluster, set a distinct service name per deployment so the traces do not merge.

`OTEL_BSP_SCHEDULE_DELAY` is interpreted in milliseconds. Setting it to `1000` makes spans arrive about a second after they end, which is useful while debugging. An unset, zero, or unparseable value falls back to the SDK default of five seconds. Lower values increase collector traffic, so leave the default for production unless you have a reason to change it.

### Apply the variables

For a chart installation, the simplest way to try tracing is to add the variables to the running DaemonSet:

```bash
kubectl -n <namespace> set env daemonset/gantry \
  OTEL_TRACES_EXPORTER=otlp \
  OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability.svc:4317 \
  OTEL_SERVICE_NAME=gantry
```

Replace `<namespace>` with the namespace that holds the Gantry release. The endpoint in this example is a plaintext in-cluster collector. For a TLS collector, use an `https://` endpoint and provide the certificate variables your collector requires.

The current chart does not expose a dedicated tracing value and its DaemonSet template sets only `GANTRY_POD_IP` and `GANTRY_CHAIR_NAMESPACE`. An environment change made with `kubectl set env` survives until the next `helm upgrade` rewrites the pod template. For a permanent change, add the same variables to your own deployment overlay or to the pod template you render, rather than editing the chart's generated objects after installation.

If you maintain your own manifest, add the variables to the `gantry` container:

```yaml
containers:
  - name: gantry
    env:
      - name: OTEL_TRACES_EXPORTER
        value: otlp
      - name: OTEL_EXPORTER_OTLP_ENDPOINT
        value: http://otel-collector.observability.svc:4317
      - name: OTEL_SERVICE_NAME
        value: gantry
```

### Confirm that it started

On a successful start, the agent writes a structured log line with the message `tracing enabled` and a `service` field holding the resolved service name. That line is the cheapest way to confirm that the variables reached the container and that the exporter was constructed.

If exporter construction fails, the agent writes `tracing disabled` at warning level with an `err` field and continues serving pulls. An observability outage is treated as an observability outage, not a data-plane outage. The same is true at shutdown: the final flush is bounded at five seconds, so a collector that stops responding cannot hold a pod open past its termination grace period. A `tracing shutdown` warning means the final flush did not complete, and spans buffered at that moment may be lost.

## Span Attributes

Attributes turn a trace from a timing diagram into a queryable record. Every Gantry span uses the OpenTelemetry semantic-convention keys where a standard key exists, such as `http.request.method` and `http.response.status_code`, and a `gantry.*` namespace for domain-specific values. The namespace keeps Gantry hops filterable without matching unrelated instrumentation in the same collector.

The mirror span carries most of the useful domain attributes. It is opened by the HTTP handler for a digest request and annotated by the serve path that eventually wins.

| Attribute | Type | Span | Meaning |
| --- | --- | --- | --- |
| `gantry.digest` | string | Mirror | The digest being served, for example `sha256:abcd...`. |
| `gantry.kind` | string | Mirror | `layer`, `manifest`, or `config`. |
| `gantry.registry` | string | Mirror, origin | Configured upstream name on the mirror; request host on the origin span. |
| `gantry.repository` | string | Mirror | OCI repository from the request path. |
| `gantry.cache.result` | string | Mirror | `hit` when the local content store served the digest. Absent otherwise. |
| `gantry.source` | string | Mirror | `local`, `peer`, or `upstream`, naming the backend that delivered bytes. |
| `gantry.bytes` | integer | Mirror | Total bytes delivered to containerd by this mirror request. |
| `gantry.media_type` | string | Mirror | OCI media type of the response, when the serving path knows it. |
| `gantry.url.redacted` | boolean | Origin | Whether any recorded URL credential was replaced. |

The values of `gantry.kind` follow the metric vocabulary, not the OCI URL vocabulary. A blob request reports `layer`, a manifest request reports `manifest`, and an image config request reports `config`. This is the same spelling used by the `kind` label on Gantry metrics, so a trace query and a metric query describe the same object without a translation table.

`gantry.source` is the single most useful attribute for diagnosis. `local` means the bytes never left the node. `peer` means they came from another Gantry agent inside the cluster. `upstream` means they came from the configured registry. The source is recorded through the same funnel that records the byte counter, so the span and the metric cannot disagree about which backend served the response.

A cached HEAD is attributed too. Containerd issues a HEAD when it already holds a partial layer, and that request has no body to count. The mirror records `gantry.cache.result=hit`, `gantry.source=local`, and `gantry.bytes=0`, which is how a trace distinguishes a cached metadata response from a miss that went to a peer.

`gantry.bytes` is a per-request total, not the size of the last chunk. A live stream can resume across peers: one provider stalls partway through a layer, the mirror abandons it, and a second provider continues from the verified offset. Each chunk increments the metric counter, because cluster-wide byte totals need each increment, while the span accumulates the running total. A resumed 57-byte layer therefore reports 57 on the span, not the 21 bytes of the final chunk.

Several attributes are conditional. `gantry.source` and `gantry.bytes` are attached when a serve path reports delivered bytes, and a cached HEAD attaches them explicitly. A request that fails before a source is chosen has neither. `gantry.cache.result` is only set to `hit`; the mirror does not write `miss` on the span. Read an absent `cache.result` as "not served locally", not as a separate miss marker. `gantry.media_type` is set on a local hit from the response header that was actually written, and may be absent on peer or origin paths.

The `gantry.peer.id` key exists in the attribute vocabulary for the peer that served or was attempted, but the current mirror and transfer spans do not attach it. When you need the peer address, use the `peer` field in the mirror's debug logs and the `gantry.peer.fetch` span's URL and timing rather than expecting a peer identity attribute on the span.

The origin client span carries a smaller set of domain attributes. It records `http.request.method`, `url.full`, `gantry.url.redacted`, `gantry.registry`, and `http.response.status_code`. It does not carry the digest, because the origin client works on a request and does not own the mirror's content identity. The `gantry.registry` value on an origin span is the host the request is actually going to, which can differ from the configured upstream name on the mirror span when a registry redirects to storage.

## Signed URLs

Many registries do not stream blob bytes themselves. They answer a blob GET with a redirect to object storage, and the storage provider authorizes the request with a pre-signed query string. Azure Blob Storage uses parameters such as `sv`, `st`, `se`, and `sig`. S3 uses `X-Amz-*` parameters. Google Cloud Storage uses `X-Goog-*` parameters. Those values are credentials: anyone who can read them can replay the request until the signature expires.

If Gantry recorded the redirect URL verbatim in `url.full`, a trace backend would become a credential store with a longer retention period than the signature. Access to the collector would be enough to replay pulls. Gantry therefore records a redacted copy of the URL and sends the original request on the wire.

The redaction happens on a clone. The recorded clone has sensitive query values replaced with the literal `REDACTED`, and URL user information removed. The wire clone keeps the original URL, including any Basic credentials in the endpoint, because that is how supported legacy endpoints authenticate. Gantry never writes the redacted URL to the socket, and never writes the original credential to the span.

`sensitiveQueryFragments` matching is substring and case-insensitive for names that are distinctive enough to be safe: `sig`, `credential`, `token`, `secret`, `password`, `apikey`, `api-key`, `expires`, `goog`, and `x-amz`. Short or ambiguous names are matched only when the whole parameter name is one of `se`, `sv`, `st`, `sr`, `srt`, `sp`, `sk`, `ss`, `spr`, or `ska`. This is what keeps ordinary parameters visible: a tags-list `last` cursor, a `first` marker, a `range` value, a `name` field, a `test` flag, and a `statement` parameter all survive because none of them is exactly a signed parameter name.

The same exact-match rule protects pagination and multipart parameters. `n`, `partNumber`, and `versionId` are preserved, so a trace can still show which page or part was requested. The redaction rule is deliberately biased toward over-redaction within the sensitive families: losing a diagnostic value costs detail, while missing a credential publishes a secret.

`gantry.url.redacted` makes the matcher observable. It is `true` when at least one query value was replaced or when URL user information was removed, and `false` when the recorded URL needed no change. A request that carries a signature-bearing parameter name but reports `gantry.url.redacted=false` is the signal that a provider changed its parameter vocabulary and the matcher no longer covers it. Without the boolean, a correctly redacted URL and a matcher that stopped firing would look identical.

Two details are worth knowing when reading a recorded URL. First, re-encoding a query re-sorts its parameters, so the order in `url.full` can differ from the order on the wire. The values and names are what matter, not the ordering. Second, the span attributes are built from a recorded clone while the outbound request is a separate clone that keeps the original URL. The traceparent injected into that outbound request comes from the same trace context, so a downstream registry continues the same trace without seeing the credential.

The redirect hop to storage is traced by the same round tripper. Because Go's HTTP client follows automatic redirects without a parent context, that storage hop appears as a separate root span rather than a child of the registry request. It is still redacted by the same code, so the signature does not appear in the trace. Requests Gantry issues itself, including the auth retry and range requests, stay in one trace.

Name-based redaction has a known limit. Matching runs on the decoded parameter name, so a name mangled into something that no longer contains a sensitive fragment can slip through. The storage providers Gantry talks to emit fixed parameter names, and Gantry follows a `Location` header rather than constructing these URLs, so this is not a provider behavior you are expected to encounter. The alternative, redacting every query value, would remove the cursors and ranges that make a trace useful.

None of this changes with tracing disabled. Redaction decides what a recorded attribute would contain, and the wire request is never modified either way. Turning tracing off removes the span, not the safety property.

## Health Checks

The mirror and transfer endpoints both expose `/healthz`, and both exclude it from tracing. The filter runs before the OpenTelemetry HTTP wrapper extracts the incoming `traceparent`, so a health probe produces no span and does not continue an inbound trace. That is intentional: there is no useful pull context to propagate through a kubelet probe.

The exclusion matters more than it looks. Gantry uses a parent-based sampler that always samples a root span. A request with no `traceparent` is therefore sampled, which is what keeps a standalone agent traceable. If `/healthz` were not filtered, every probe interval on every node would export a root span and a matching batch to the collector. That is a steady stream of traffic unrelated to image pulls, and it would dominate both the collector and any trace query that filters only by service name.

The readiness and liveness probes in the chart use `/readyz` and `/livez` on the metrics listener, not the mirror or transfer listener. Those handlers are not wrapped by the mirror or transfer instrumentation, so they do not produce spans. There is no dedicated probe span name to search for, and no per-probe collector traffic to suppress.

## Sampling

Gantry installs a `ParentBased(AlwaysSample())` sampler. The parent-based part preserves a sampling decision that containerd already made. If containerd sampled the pull, Gantry records its spans as children of that trace. If containerd decided not to sample, Gantry does not restart the trace or force a sample; the context is still propagated, but no spans are exported from Gantry.

The always-sample part applies only to a root span, which is a request that arrives without a `traceparent`. That makes an agent traceable on its own, without requiring containerd instrumentation, and it makes a direct request to the mirror during debugging visible immediately.

The W3C TraceContext propagator is installed even when the exporter is off. This is load-bearing rather than defensive. The HTTP instrumentation falls back to the global propagator, and an empty propagator would suppress both injection and extraction. Keeping the propagator live decouples "am I recording?" from "do I carry trace context?". It lets an agent forward an existing trace when a collector is configured but the incoming sampling decision says skip, and it keeps a request chain intact across an agent that is not exporting.

The sampling policy is fixed in code. Standard sampler environment variables such as `OTEL_TRACES_SAMPLER` are not read, so changing them has no effect on Gantry. If you need to reduce trace volume, reduce it at the collector or point only selected agents at a collector, rather than expecting a sampler environment variable to change the decision.

## Both Backends

Tracing is initialized on the common agent startup path, before the backend dispatch. That ordering is deliberate. Both the classic containerd-backed agent and the Racer-backed agent construct an origin client and a mirror server, and both need the same tracer provider and the same W3C propagator.

If tracing were initialized after the Racer branch, the Racer backend would build its components against the default no-op provider. It would export nothing, and because the propagator would also be missing, it could not even forward an existing trace context to a peer or registry. Initializing before the branch keeps both backends on one code path and makes the span names and attributes identical regardless of which backend is active.

The context passed to initialization scopes only exporter construction, not the agent's lifetime. The OTLP connection outlives that context, and the final flush is handled by a deferred shutdown on the startup path. With tracing disabled, the same call installs a no-op provider and an inert shutdown, so a default agent neither dials a collector nor pays span costs.

The practical consequence is that there is one place to configure tracing and one set of span names to learn. You do not need different environment variables or a different collector pipeline for the Racer backend.

## Reading a Trace

Start at the root and read downward. In a fully instrumented cluster the root is the kubelet or containerd `PullImage` span; in a cluster where only Gantry exports traces, the root is the mirror span. Either way, the pull is a tree of digests, not one linear operation: a manifest request, then a config request, then one request per layer.

The first thing to check is whether a mirror span exists for the digest you care about. The span name is `gantry.mirror.serve_digest` for a sha256 digest path. A request to `/v2/` or a tag request produces the generic `gantry.mirror` name and no digest attributes, because the mirror does not serve those requests from content. If you see no mirror span at all, the request did not reach Gantry, which points at the containerd `hosts.toml` configuration, an unconfigured upstream, or a trace exporter that is not running.

The second thing to check is `gantry.source`. It tells you which branch of the mirror ran and therefore which child spans to expect. `local` should have no children, `peer` should have one `gantry.peer.fetch` child per attempt, and `upstream` should have one `gantry.origin HEAD` or `gantry.origin GET` child per HTTP attempt, including the auth handshake and range requests.

The third thing to check is duration attribution. Compare the mirror span's duration with its children rather than reading the total in isolation. If a 4 second mirror span contains a 3.9 second peer fetch, the time is in the peer path. If it contains a 3.9 second origin GET, the time is upstream. If it contains a 50 millisecond child but still took 4 seconds, the time is in the local store, in response headers, or in the body copy after the child completed.

The fourth thing to check is `gantry.bytes` against the size you expected. A span that reports fewer bytes than the layer contains did not deliver a complete object. That is normal for a transfer that stalled and was retried, and it is a strong signal to look for a later retry of the same digest on the same node. If the pull ultimately succeeded, the successful retry has its own span and its own byte count.

Error status is not uniform across span types. The origin client marks any HTTP status of 400 or above as an error and also marks a body read failure, because a truncated download never surfaces as a transport error after the headers have arrived. That means the expected 401 during an authentication handshake appears as an error span even though the pull continues normally. The mirror and transfer server spans mark 5xx responses as errors through the standard HTTP instrumentation. Peer client spans do not derive status from the HTTP code, so a 404 normally appears as a non-error client span with `http.response.status_code=404`. Read the status code rather than assuming that every 404 is an incident.

Redirects are not errors. The origin client leaves 3xx spans unset and lets the HTTP client follow the redirect. The follow-up request to storage is traced by the same round tripper, but as a separate root span, so a storage hop can appear as its own trace rather than a child of the registry request.

A trace viewer query is usually easiest to build from the service name and the span name. Select `service.name=gantry`, then filter to `gantry.mirror.serve_digest`, then add `gantry.digest` or `gantry.repository` to narrow to one image. If your backend supports attribute grouping, group by `gantry.source` to see how a fleet split between local, peer, and upstream for the same layer.

Keep clock skew in mind when comparing spans from different nodes. A peer fetch and its transfer serve child run on different clocks, and a few milliseconds of skew can make a child appear to start before its parent. The durations are still useful; the absolute ordering across nodes is less reliable than the ordering within one process.

## Reading a Cold Start

A cold start is the case tracing explains best: a multi-layer image lands on a node whose content store is empty, and the trace shows where each digest came from as the swarm fills in.

The first manifest request on an empty node usually has `gantry.source=upstream` and a `gantry.origin GET` child. The mirror has no local copy and no peer provider, so it goes to the registry. That span covers the registry round trip and the body read, which is why it can be long even when the registry responds quickly; the content-store commit is visible in the enclosing mirror span. The config blob and the first layers follow the same pattern until a provider appears.

A layer that the node already holds appears as a mirror span with `gantry.cache.result=hit`, `gantry.source=local`, and no children. A zero-byte HEAD hit has the same shape with `gantry.bytes=0`. A layer served by a peer appears as `gantry.source=peer` with a `gantry.peer.fetch` child, and the seeding node contributes a `gantry.transfer.serve` child under that fetch when tracing is enabled on that node too. A swarm that seeds different layers from different nodes produces one such pair per layer, which is the fanout a cold-start trace is meant to show.

Cold-start pulls coordinated through a chair need one caveat. The chair receives a `please_pull` request and performs the origin pull in its own process. Gantry does not propagate trace context across the chair RPC, so the chair's `gantry.origin GET` span appears as a separate trace rooted at the chair, not as a child of the requester's mirror span. The requester's mirror span can therefore be long and childless while it waits for a provider to appear. When you see that shape, look at the chair's trace and at the cold-start metrics rather than concluding that the mirror did no work.

A rollout produces a mix of sources on purpose. Nodes that already have a layer serve it locally. Nodes that can see a provider fetch from that provider. Nodes with an empty or unhealthy DHT view fall back to the registry. Filtering the same layer across nodes by `gantry.source` shows whether the swarm is actually helping or whether most nodes still pay the upstream cost. A sudden shift from `peer` to `upstream` across many nodes usually points at provider records expiring, a DHT health drop, or a restart that emptied the local store.

Cache seeding failures are visible as a mismatch between what a span says was delivered and what later spans say is local. If a peer fetch reports fewer bytes than the layer contains, the writer may not have committed the object, and the next request for that digest will miss locally and fetch again. A `gantry_mirror_response_completed_timestamp_seconds` series that never advances for that kind and source is the metric-side version of the same symptom. The trace tells you which attempt was short; the metric tells you whether a complete response was ever recorded.

`p2p_dht_health_score` is the companion signal for the cold-start path. A low score means the agent's routing table, lookup latency, and self-test are not healthy enough to trust a peer lookup, which pushes pulls toward the origin and makes cold starts slower. Read it next to `p2p_dht_lookup_duration_seconds`, `p2p_origin_pull_queue_wait_seconds`, and `p2p_cold_start_duration_seconds` when a trace shows a long childless mirror span during startup.

## Correlating With Logs

A trace tells you which digest and which source, and the logs tell you what the code decided around that digest. The mirror builds a per-request logger with `registry`, `repo`, `digest`, and `kind` fields, so its messages can be matched to a span without guessing from free text.

The agent does not write the trace ID into its structured logs. Correlate by the digest and the time window instead: take `gantry.digest` and the span start and end times from the viewer, then search the agent's logs for that digest around that interval. The same fields appear in warning and debug messages about peer selection, origin failures, cache commits, and digest mismatches.

For a per-request investigation, raise the log level to `debug` and restart the affected agent, or use the `--log-level=debug` flag in a local run. The flag is registered by the agent and accepts `debug`, `info`, `warn`, or `error`. Debug logging adds the per-request decisions that a trace summarizes, such as which providers were filtered, which peer attempt failed, and whether a cache write succeeded. Remember to lower it again; debug logs are voluminous at fleet scale.

```bash
kubectl -n <namespace> logs -l app.kubernetes.io/name=gantry --tail=200
```

The `peer` field on mirror messages carries the peer address for a peer fetch. Use it with the `gantry.peer.fetch` span to identify which node was slow, then search that node's logs for the same digest. For the Racer backend, the mirror also sets a `Gantry-Racer-Request-ID` response header that can be used to follow a request through Racer's own logs.

At startup, the `tracing enabled` message with its `service` field confirms the identity the agent reports to the collector. A missing line means the environment variables did not reach the container, or the exporter failed and the `tracing disabled` warning is the line to read instead.

## Correlating With Metrics

Gantry serves Prometheus metrics on the metrics listener, which defaults to `0.0.0.0:9095`, at `/metrics`. The chart also exposes that port as a container port named `metrics`. Metrics answer fleet-wide questions that a single trace cannot, such as whether an unexpectedly slow pull is an outlier or the normal path for every node.

| Metric | Labels | Use |
| --- | --- | --- |
| `p2p_cache_hit_total` | none | Local content-store hits. |
| `p2p_cache_miss_total` | none | Local misses, including misses later served by a peer. |
| `gantry_mirror_bytes_served_total` | `kind`, `source` | Bytes delivered by the mirror, by content kind and path. |
| `gantry_mirror_response_completed_timestamp_seconds` | `kind`, `source` | Last complete response per path. |
| `p2p_peer_fetch_total` | `outcome` | Peer fetch attempts and terminal outcomes. |
| `p2p_peer_fetch_duration_seconds` | `outcome` | End-to-end peer fetch latency. |
| `gantry_peer_fetch_bytes_total` | `kind` | Bytes received from peers, including partial transfers. |
| `gantry_peer_serve_bytes_total` | `kind` | Bytes this agent sent to peers. |
| `gantry_transfer_bytes_total` | `source`, `kind` | Bytes received during origin or peer GETs, updated while reading. |
| `gantry_active_transfers` | `source`, `kind` | Origin or peer GETs currently in progress. |
| `p2p_origin_pull_total` | `kind` | Origin pulls started. |
| `p2p_origin_pull_duration_seconds` | `kind` | Chair-side origin pull duration. |
| `p2p_origin_pull_queue_wait_seconds` | `kind` | Time an admitted origin pull waited for a slot. |
| `p2p_origin_pull_failure_total` | `kind`, `class` | Terminal origin failures by class. |
| `gantry_origin_bytes_total` | `kind` | Bytes read from upstream registries. |
| `p2p_dht_lookup_duration_seconds` | `outcome` | Provider lookup latency. |
| `p2p_dht_health_score` | none | DHT health in the range 0 to 1. |
| `p2p_cold_start_duration_seconds` | `digest_kind`, `outcome` | Cold-start duration. |

The metric and trace vocabularies differ on purpose. Trace `gantry.source` uses `local`, `peer`, and `upstream`. The `source` label on `gantry_mirror_bytes_served_total` uses `cache`, `peer`, and `origin`. The mapping is fixed: `cache` is `local`, `peer` is `peer`, and `origin` is `upstream`. The metric spelling is an existing dashboard contract and is not changed to match the span spelling.

The units differ too. A metric counter receives incremental byte counts, while a span receives the accumulated total for one request. A resumed stream can call the byte funnel more than once, so two counter increments may correspond to one `gantry.bytes` value. That is why a counter query can show more increments than there are spans; it is not double counting.

A useful correlation sequence is: find the slow span, note its digest, source, and node, then check whether the matching metric series moved for that source at the same time. If the span says `local` and `p2p_cache_hit_total` is rising normally, the local store is serving but the disk is slow. If the span says `peer` and `p2p_peer_fetch_total{outcome="stall"}` spiked, the peer path is failing over rather than simply being slow. If the span says `upstream` and the origin failure counter is flat, the registry is reachable but slow, which is a different problem from a registry that is rejecting requests.

## Troubleshooting Slow Pulls

Use the span shape first, then confirm with a metric. The table below maps the common shapes to the next check.

| Symptom in the trace | Likely area | Next check |
| --- | --- | --- |
| Long mirror span, no child, `source=local` | Local content store or disk | The node's disk latency and containerd store metrics such as `gantry_containerd_open_error_total`. |
| Long mirror span, no child, no `source` | Failed or waiting before a source was chosen | The mirror warning log for the digest and the upstream status. |
| Long `gantry.peer.fetch`, short `gantry.transfer.serve` | Network path to the peer | Transfer port reachability and `gantry_active_transfers{source="peer"}`. |
| Long `gantry.transfer.serve` | Seeding node's disk or CPU | That node's own logs and `gantry_peer_serve_bytes_total`. |
| Several `gantry.peer.fetch` spans for one digest | Stall, digest mismatch, or busy peer | `p2p_peer_fetch_total` by `outcome`. |
| `source=upstream` when peers should exist | Discovery or advertisement | `p2p_dht_health_score`, lookup latency, and stale-provider counters. |
| Long `gantry.origin GET` | Registry, storage, or network to them | `p2p_origin_pull_duration_seconds`, queue wait, and failure class. |
| `gantry.bytes` below the layer size | Truncated or failed transfer | The next retry span and the mirror warning log. |
| No mirror span for a pull | Request bypassed Gantry | `hosts.toml`, the upstream allowlist, and the unconfigured-registry warning. |

A childless local span is not automatically a problem. A cached layer should be fast, and a short childless span is the expected shape. Investigate it only when the duration is large relative to the layer size or when it repeats across nodes. The same applies to a zero-byte HEAD hit: it is a metadata response, not a delivery, and its duration is dominated by the content-store lookup.

A long childless mirror span during startup needs special handling. If the request is waiting on a chair's cold-start pull, the origin work is in the chair's trace, not the requester's. Check the chair's logs and the cold-start metrics before assuming the mirror is stuck. If the request is in a direct-origin fallback, the origin span should be present as a child; its absence points at an early failure or at tracing being disabled on that agent.

If the mirror span is missing entirely, confirm that the request reached the mirror at all. A registry that is not listed in `upstream_registries` is rejected with a warning that names the request, and containerd then falls through to the next host. A tag request is answered with 503 by design so containerd retries against the next mirror. Neither case produces a digest span, because there is no digest to serve.

If spans from one node never appear while other nodes work, the problem is local to that agent. Check for the `tracing enabled` startup line, confirm that `OTEL_TRACES_EXPORTER` is exactly `otlp`, and look for a `tracing disabled` warning. A collector that is reachable from some nodes but not others usually points at a NetworkPolicy, a DNS name that does not resolve in that namespace, or a TLS trust difference.

Finally, do not mistake batch delay for pull latency. Spans are batched with a default five second schedule delay, so a trace can appear several seconds after the request finishes. The span timestamps still describe the request accurately. If the trace backend shows a gap between the pull time and the trace time, check `OTEL_BSP_SCHEDULE_DELAY` before investigating the data plane.

## Related

- [Gantry]({{< relref "guides/gantry" >}}) for the pull path, endpoints, and deployment model.
- [Benchmark Gantry]({{< relref "guides/gantry-benchmark" >}}) for the metrics methodology behind the performance claims.
- [Architecture]({{< relref "reference/architecture" >}}) for how Gantry fits into the wider Unbounded control plane.
