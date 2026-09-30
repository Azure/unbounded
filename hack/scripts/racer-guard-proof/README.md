# Pre-established guard proof collector

`dynamic-preopen.go` preserves the stage47 operational collector as a standalone
Go helper using the repository's existing dependencies. It is not a generic
cluster tool: its context, namespace, selector, and inventory size are deliberate
operational constants. No new module or dependencies are required.

The stream launcher passes each pod, pipe reader, and release channel directly
to its worker before launching it. Workers never look up either setup map.
The owner closes release channels on ARMED and sends GO only after all streams
arm. Capture commands, proof checks, security TTLs, and resource limits are
unchanged from the operational helper.

Local tests do not contact Kubernetes:

```sh
timeout --signal=TERM --kill-after=10s 60s go test -race -timeout=5m ./hack/scripts/racer-guard-proof -count=1
```

`TestCapturePipes` exercises the production launch loop with empty, single-stream,
and 1,500-stream fixtures, checking per-node release-channel and pipe identity.
Before the snapshot fix the same fixture reports a data race and can terminate
with concurrent map read/write. The closed-pipe case covers abort before GO.

A passing local test is not a fleet qualification. Any live capture requires
separate authorization, fresh operational preflight, and current-time fleet
verification. Never replay a consumed full-run authorization.
