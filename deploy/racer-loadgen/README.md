# Racer load generator

Standalone development load generator for an **already functioning Racer-enabled
Gantry deployment**. Each DaemonSet pod runs a deterministic synthetic OCI origin
on port 8080 and HTTP pull workers targeting its node's Gantry on port 5000.
Metrics and health endpoints use port 9090. These manifests are applied directly
with Kustomize; they are not an Unbounded operator component.

## Prerequisites and routing

- Gantry and Racer must already serve successful digest-addressed pulls. Gantry
  needs `GANTRY_RACER_ENABLED=true`, a Racer cache named `gantry`, and working
  `/run/racer/gantry/client/socket` and `/run/racer/gantry/origin/socket` paths.
  See [Gantry provisioning](../gantry/README.md#provisioning). The stock Gantry
  chart does not enable Racer or mount its sockets
  (`deploy/gantry/chart/templates/daemonset.yaml:71-79`, `:124-154`); these manifests
  assume your existing deployment handles that. Socket checks alone do not prove
  object serving (`cmd/gantry/agent_racer.go:209-219`).
- The default Kubernetes namespace is the existing `unbounded-system`. Deploy
  these resources in the same namespace as Gantry: a selector-based Service
  cannot select pods in another namespace. The base does not create or own that
  shared namespace.
- The target Service selects `app.kubernetes.io/name: gantry` and
  `app.kubernetes.io/component: agent` on pod port 5000
  (`service-gantry.yaml:8-16`). Adjust the selector to your agents and ensure
  Gantry listens on its pod interface.
- Both Services use `internalTrafficPolicy: Local`. A request has **no usable
  endpoint when its source node has no ready local matching pod**; Kubernetes
  does not fall back to a remote node. Every loadgen node needs a ready Gantry
  agent, and every Gantry node that may execute an origin callback needs a ready
  loadgen origin. Align coverage across all participating Gantry/Racer nodes.
- The DaemonSet selects Linux nodes and supplies no extra tolerations
  (`daemonset.yaml:23-34`). Choose selectors and tolerations for your benchmark
  nodes; Gantry's chart tolerates all taints (`deploy/gantry/chart/values.yaml:16-17`).
- Cluster networking must support Local internal Service routing and permit
  loadgen-to-Gantry TCP 5000, Gantry-to-origin TCP 8080, DNS, and metrics scraping
  on TCP 9090.

## Local baseline

From the repository root, use a Go toolchain compatible with `go.mod`. This runs
two workers against the process's own origin with two 1 MiB payload layers and a
30-second load phase, without Gantry or Racer:

```sh
go run ./cmd/racer-loadgen \
  --listen=127.0.0.1:8080 --metrics-listen=127.0.0.1:9090 \
  --target=http://127.0.0.1:8080 --namespace= \
  --layers=2 --layer-bytes=1048576 --jitter=0 \
  --concurrency=2 --layer-concurrency=2 --interval=100ms \
  --start-delay=0 --duration=30s
```

While it runs, inspect `curl -fsS http://127.0.0.1:9090/metrics` in another terminal,
or scrape that endpoint every 5 seconds with Prometheus for the queries below.
The metrics endpoint closes when the process exits; pull failures are in metrics,
not in the process exit status (`cmd/racer-loadgen/pull.go:93-114`,
`cmd/racer-loadgen/main.go:176-206`). Use `--concurrency=0` for origin-only mode.

## Build and deploy

Build the container using the repository root as the context, for your nodes'
architectures:

```sh
docker build -f images/racer-loadgen/Containerfile -t racer-loadgen:dev .
```

Load `racer-loadgen:dev` into every selected node's image store, or push an image
to a registry reachable by those nodes:

```sh
docker tag racer-loadgen:dev registry.example.com/dev/racer-loadgen:benchmark-v1
docker push registry.example.com/dev/racer-loadgen:benchmark-v1
```

Override the image through the `images` entry in `kustomization.yaml`:

```yaml
images:
  - name: racer-loadgen
    newName: registry.example.com/dev/racer-loadgen
    newTag: benchmark-v1
```

The base resolves to `racer-loadgen:dev` with `IfNotPresent`. Use a fresh tag or
digest for changed builds so nodes do not reuse a previous local image.

### Configure the existing Gantry upstream

Add this entry to the existing Gantry configuration's `upstream_registries`,
preserving its other upstreams, and roll out that configuration through its
current deployment mechanism:

```yaml
upstream_registries:
  - name: loadgen.invalid
    endpoint: http://racer-loadgen-origin.unbounded-system.svc.cluster.local:8080
```

This read-only synthetic origin requires no credentials. `loadgen.invalid` is
the registry name sent in the pull request's `ns` query parameter, not a DNS name
to resolve or a Kubernetes namespace (`cmd/racer-loadgen/pull.go:215-220`).

### Apply and customize

```sh
kubectl kustomize deploy/racer-loadgen
kubectl apply -k deploy/racer-loadgen
kubectl -n unbounded-system rollout status daemonset/racer-loadgen --timeout=30m
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=racer-loadgen -o wide
kubectl -n unbounded-system get endpointslices \
  -l kubernetes.io/service-name=racer-loadgen-gantry
kubectl -n unbounded-system get endpointslices \
  -l kubernetes.io/service-name=racer-loadgen-origin
```

Use a Kustomize overlay or edit this development base to customize:

- **Namespace:** change `namespace` in the kustomization to Gantry's namespace
  and update the upstream origin FQDN above (and the cluster domain if needed).
  The short target name `racer-loadgen-gantry` still resolves in the pod namespace.
- **Gantry selector:** patch `service-gantry.yaml` if your existing agents use
  different labels. Do not apply a global selector-changing loadgen label
  transformer to this Service; its selector deliberately identifies Gantry.
- **Scheduling:** patch `spec.template.spec.nodeSelector` and `tolerations` in
  the DaemonSet to align origin coverage and local Gantry endpoints.
- **Workload:** change container `args`. Kustomize strategic merge patches
  replace the entire args list, so retain every setting you want to keep.
- **Resources:** the base requests 250m CPU and 128Mi memory, with no fixed
  limits. Size requests and any limits for the experiment. CPU limits can cap
  the load generator before the system under test saturates.

`/readyz` on port 9090 reflects **origin availability only**, not successful
Gantry pulls (`cmd/racer-loadgen/main.go:109-119`, `:176-180`). Keep it independent
of the target so failures do not remove the origin Gantry needs. `/healthz` is
liveness. The startup probe allows 180 attempts spaced 10 seconds apart for image
generation and hashing (`daemonset.yaml:60-78`); increase that budget for larger
images or constrained CPUs. The 10-second start delay is not a cluster-wide
readiness barrier.

## Workload and repeatability

The base uses 8 layers with 64 MiB base payloads and +/-20% size jitter, 64 image
workers, 4 layer requests per pull, SHA-256 verification, and a 2-minute pull
timeout (`daemonset.yaml:38-54`). See `go run ./cmd/racer-loadgen --help` for all
flags. `--duration=0` runs indefinitely; a positive duration starts after image
initialization and `--start-delay` (`cmd/racer-loadgen/main.go:176-192`). A
DaemonSet restarts an exited container, so finite duration is not a one-shot Job.

Each pod generates the same fake but valid OCI image: uncompressed tar layers
with deterministic pseudorandom payloads, a Linux/amd64 config, and no runnable
application. Do not use it as a workload container. `--layer-bytes` excludes tar
headers, padding, and end blocks, so actual object sizes are larger than the
jittered payloads (`cmd/racer-loadgen/image.go:92-94`, `:120-146`, `:174-195`).

Keep seed, layer count, layer bytes, jitter, repository, and registry namespace
uniform across all nodes. Layer bytes are generated on demand with bounded
buffers, but startup must hash every layer to establish its digest
(`cmd/racer-loadgen/image.go:83-123`, `:198-239`). For cold-content experiments,
change the seed everywhere; restarting with the same seed does not make the
cache cold. Roll out new settings with `--concurrency=0`, wait for all origins,
then enable workers with identical settings and wait again before measuring.
Mixed origins can reject unknown digests (`cmd/racer-loadgen/registry.go:50-68`),
and rolling updates temporarily remove each node's Local origin endpoint.

There is no containerd cache in the loadgen client. Each successful pull fetches
and fully consumes the manifest, config, and every layer again, even on warm
Racer hits. The client discards response bodies after counting bytes and optional
SHA-256 verification (`cmd/racer-loadgen/pull.go:153-180`, `:240-255`, `:261-301`).
The timeout covers a whole pull; failures wait `--retry-delay`, and successes wait
`--interval` (`cmd/racer-loadgen/pull.go:93-114`, `:133-135`).

Concurrency counts whole-image workers; layer concurrency is per pull. The
defaults can offer up to 64 * 4 concurrent layer requests per node, but **Gantry's
Racer SDK client defaults to 16 simultaneous connections/live object streams**
(`pkg/racersdk/client.go:58-60`, `:76-80`, `:142-157`). Gantry uses that default in
`cmd/gantry/agent_racer.go:49`. More HTTP workers can therefore measure queuing
and hit pull timeouts rather than increase Racer stream concurrency.

Origin generation and pull workers share one pod and CPU budget, alongside
Gantry/Racer on the node. Cold runs include origin generation CPU; warm runs
still include client body reads and, with `--verify=true`, hashing. Compare
`--verify=false` if client CPU is the bottleneck: it skips client digest hashing,
but still consumes bodies and checks lengths (`cmd/racer-loadgen/pull.go:240-255`,
`:268-295`). Record that setting with results and monitor node/process CPU before
attributing throughput limits to Racer.

## Metrics

Scrape `/metrics` on pod port 9090. The pod annotations require a Prometheus setup
that discovers annotated pods (`daemonset.yaml:19-22`). Metrics are defined in
`cmd/racer-loadgen/metrics.go:27-43`; histograms export `_bucket`, `_sum`, and `_count`.

| Metric | Meaning |
| --- | --- |
| `racer_loadgen_pulls_total{result}` | Completed whole-image pull attempts. |
| `racer_loadgen_pull_duration_seconds{result}` | Histogram of whole-pull duration, excluding inter-pull delay. |
| `racer_loadgen_in_flight` | Currently active whole-image pulls, not layer requests or sleeping workers. |
| `racer_loadgen_received_bytes_total` | Client response-body bytes read, including partial and failed responses; excludes HTTP headers. |
| `racer_loadgen_requests_total{kind,result}` | Completed client HTTP request attempts. `kind` is `manifest`, `config`, or `layer`. |
| `racer_loadgen_request_duration_seconds{kind,result}` | Histogram of client request duration including body consumption. |
| `racer_loadgen_origin_requests_total{method,code}` | Origin HTTP requests by method and response status. |
| `racer_loadgen_origin_bytes_total` | Origin response-body bytes written, excluding HTTP headers. |
| `racer_loadgen_origin_request_duration_seconds` | Histogram of origin request duration. |

Client `result` is `success`, `error`, or `canceled`; deadline expiration is an
error (`cmd/racer-loadgen/pull.go:183-193`). Add scrape-label filters such as `job`
or `namespace` to isolate your run. These queries aggregate across selected pods;
use a rate window with several scrapes (extend `[1m]` for slower scraping):

```promql
# Successful whole-image pulls per second
sum(rate(racer_loadgen_pulls_total{result="success"}[1m]))

# Received response-body bytes per second, including failed/partial pulls
sum(rate(racer_loadgen_received_bytes_total[1m]))

# p95 successful whole-image pull latency in seconds
histogram_quantile(0.95,
  sum by (le) (rate(racer_loadgen_pull_duration_seconds_bucket{result="success"}[1m]))
)
```

Use successful pull rate for completed-image throughput; the byte counter is not
success-only (`cmd/racer-loadgen/pull.go:275-294`). Origin bytes measure a different
leg and need not equal client bytes. Check errors alongside success-only p95;
neither a fast p95 nor high byte traffic alone proves successful pulls.

## Remove

```sh
kubectl delete -k deploy/racer-loadgen
```

Also remove the synthetic upstream entry from the existing Gantry configuration
when finished.
