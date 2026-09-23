<!-- Copyright (c) Microsoft Corporation. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# RACER control plane

Kubernetes topology controller, mTLS protobuf control server, and durable CA
manager for the RACER dataplane, with Site-derived membership and `/v3`
subscriptions.

Build from the Unbounded repository root with Go 1.26.6:

```sh
GOTOOLCHAIN=go1.26.6 go build -mod=readonly -o bin/racer-controlplane ./cmd/racer-controlplane
```

Run using the operator-managed Deployment described below. Serving requires
Kubernetes credentials, `RACER_POD_NAME` and `RACER_POD_UID`, and a live managed
Pod -> ReplicaSet -> `racer-controlplane` Deployment ownership chain for certificate
issuance. A standalone `go run` with only `KUBECONFIG` is not a serving replica.

The package uses the root module's Kubernetes v0.37.0 and controller-runtime
v0.25.1 dependencies. Protobuf definitions and generated Go bindings belong to
[`api/racer`](../../api/racer); regenerate there, not in this command.

## Kubernetes inputs

Enable the operator-managed deployment with `spec.components.racer.enabled: true`
on a Site. An omitted Racer block, omitted `enabled`, or `enabled: false` does not
enable deployment. Within an enabled Site, Nodes enroll by default without a Racer
opt-in label:

- `unbounded-cloud.io/site` selects the Node's Site. Its presence wins even when
  its value is empty or conflicts with the deprecated label.
- `net.unbounded-cloud.io/site` is used only when the canonical label is absent.
- No Site means no universe. There is no implicit `default` universe; a Site
  actually named `default` is an ordinary explicitly assigned Site.
- `racer.unbounded-cloud.io/exclude: "true"` excludes the Node from active
  membership. Only that exact label value excludes it. Removing the label permits
  re-enrollment. Node universe labels and annotations do not assign membership.

`internal/racer.UniverseForSite` supplies the shared universe mapping. A Site name
that is a valid Kubernetes label value is preserved. Otherwise the result is
`site_` followed by the lowercase, unpadded base32 SHA-256 of the Site name. This
is a label value, not a Kubernetes resource name. Node Site labels themselves
must satisfy Kubernetes label-value restrictions.

A volume is a selector-based Service with the following annotation prefix:

| Annotation (`racer.unbounded-cloud.io/`) | Default | Meaning |
| --- | --- | --- |
| `origin-service` | required | Separate origin Service name |
| `origin-namespace` | volume namespace | Origin Service namespace |
| `origin-port` | required | TCP Service port name or number, not targetPort |
| `universe` | required | Exact mapped Site universe, also used in the Pod label and Service selector |
| `slot-count` | `131072` | Fixed slots, 1-262144; immutable per volume identity |
| `listener-port` | allocated | Optional immutable port, 1024-65535 |
| `cache-generation` | `1` | Unsigned 64-bit dataset/cache generation |
| `routing-algorithm` | `2` | Only canonical destination-rooted routing is supported |
| `max-candidate-attempts` | `3` | Bounded fallback attempts, 1-8 |

For a Site named `site-a`, set the Service annotation and selector explicitly:

```yaml
metadata:
  annotations:
    racer.unbounded-cloud.io/universe: site-a
    racer.unbounded-cloud.io/origin-service: origin
    racer.unbounded-cloud.io/origin-port: "8080"
spec:
  selector:
    racer.unbounded-cloud.io/dataplane: "true"
    racer.unbounded-cloud.io/universe: site-a
```

Use the mapped value in all three places: the Service annotation, the Service
selector, and the dataplane Pod universe label. Do not use a Site UID or the
64-character cryptographic universe ID here. Missing annotations do not fall
back to a namespace, selector, Node annotation, or `default`.

The controller watches labeled dataplane Pods. Participants need an eligible Ready Node
and a selected, DaemonSet-controlled, Running Pod with an IP. Pod readiness is
not required for initial configuration. Selected Pods must belong to the volume
Service's namespace and their Nodes must belong to its universe. Kubernetes
EndpointSlices independently exclude unready Pods from client traffic. Excluded,
unassigned, and terminating Pods do not block removal generations. A live foreign
Pod selected by a conflicting Service is rejected unless it is the retained
historical recipient being drained.

When a universe has no volume Services, the controller discovers idle managed
Pods in its state namespace using the dataplane/universe labels and the
`racer-dataplane` service account. The same eligible Ready Node, Running Pod,
DaemonSet ownership and deterministic rollout selection apply. Enrollment uses
Pod-token checks; subscriptions use the enrolled certificate. Their snapshots
set `idle`, permitting readiness after worker activation without listeners.
Last-volume deletion, controller restart, and replacement
Pods use the normal durable rollout protocol. Historical, excluded, moved, or
unavailable recipients retain empty removal snapshots with `idle` unset.
Snapshots without explicit idle authorization remain unready without listeners.

Volume identity is `namespace/name`. Service recreation retains that identity;
bump `cache-generation` when replacing the dataset. Origin identity includes its
namespace, Service name, and resolved Service port. Origins must be live,
non-headless ClusterIP Services, separate from any RACER volume Service. Their
ClusterIPs must cover participating Pods' primary IP families. Numeric IPv4/IPv6
endpoints are resolved through the Kubernetes API, without dataplane DNS lookup.
Origins in other namespaces or universes are supported.

The controller patches volume Services with `internalTrafficPolicy: Local`,
`externalTrafficPolicy: Local` for NodePort/LoadBalancer, and the allocated
listener `targetPort`. Clients need a ready local endpoint. Controller-owned
output annotations are `allocated-port`, `universe-id`, and `status` under the
same prefix. Published status means configuration was published; dataplane
activation determines readiness. Invalid updates retain the last committed
generation and routing fields and record a diagnostic in `status`.

Automatic listener allocation uses 10000-29999. Reservations persist after
deletion. Ports 9090 (management) and 9443 (peer mTLS) are always reserved. Supply
`-reserved-management-ports=9090,10000` if a dataplane uses an additional
management port. Include old and new ports during a rollout. Existing allocations
cannot move; a conflicting volume needs a new Service identity with a safe port.

The Node's `racer.unbounded-cloud.io/fabric` annotation is copied into snapshots
for dataplane RDMA eligibility. Omit it for HTTP-only operation. A matching fabric
name alone does not validate hardware connectivity.

## Bootstrap and controller lifecycle

Bootstrap uses the controller binary, a Node name, the Pod's mapped Site universe,
and the Pod's primary IP. For example, inside the bootstrap container:

```sh
POD_IP="$POD_IP" racer-controlplane \
  -bootstrap-node="$NODE_NAME" -bootstrap-universe="$POD_UNIVERSE" \
  -bootstrap-namespace=racer-system -bootstrap-service=racer-controlplane \
  -bootstrap-port=8443
```

This prints shell exports for `RACER_UNIVERSE`, `RACER_NODE`, and
`RACER_CONTROL_ADDRESS`. It requires a Node UID, rejects excluded, deleting, or
unassigned Nodes, and checks the Site-derived universe against the required
`-bootstrap-universe` value. It reads the controller Service and selects a numeric
ClusterIP endpoint matching `POD_IP`. Managed subscriptions use the controller's
Service DNS name; `RACER_CONTROL_SERVER_NAME` pins its certificate DNS identity.

Identities are lowercase SHA-256 hex digests of these exact byte strings:

```text
universe: "racer/universe/v1\x00" || mapped_site_universe
node:     "racer/node/v1\x00"     || kubernetes_node_uid
```

Here `\x00` is one NUL byte. Kubernetes metadata uses the new Unbounded prefix;
cryptographic domains remain unchanged. Pod replacement retains Node identity.
Node deletion/recreation changes identity and leaves the old recipient's removal
history. Site reassignment requires replacement dataplane Pods with the new
universe label and freshly validated bootstrap. The former Pod stays authorized
only in its historical universe to receive empty removal commands. Its Pod UID
is retained until an uncached GET of its persisted namespace/name returns NotFound
or a different UID, proving actual deletion;
termination, exclusion, selector changes, and cache disappearance do not prove it.
Checks are bounded by distinct inactive recipient Pod keys, with no Pod LIST
fallback. Existing format-2 generations remain readable: UID-only members retain
authority until an observed Pod with that exact UID supplies its namespace/name.
Legacy recipients never observed again remain retained, including their history;
absence from the cache cannot safely migrate or release them.
Node replacement similarly retains the old identity as a tombstone and does not
adopt its still-running Pod into the new Node identity.

The dataplane slab has a process-lifetime exclusive `flock`. During replacement,
the old process must release the slab before the replacement can open it. Preserve
the existing slab path and normal process teardown; deleting or replacing the slab
file to bypass contention would defeat that lifecycle protection. Disabling a
Site's Racer deployment removes its dataplane; shared control-plane and CA state
are retained for removal delivery and subsequent re-enrollment.

Only the elected leader serves subscriptions, enrollment, and dataplane trust
proofs. Every replica serves its own replica-proof endpoint. `/readyz` and
`/healthz` use `-health-listen` (default 8081); readiness selects the TLS-serving
leader, while standby liveness can be healthy. The default leader-election and
durable-state namespace is `racer-system`; the operator sets its own namespace,
normally `unbounded-system`.

| TCP port | Endpoint | Transport and role |
| --- | --- | --- |
| 8443 | `GET /v3/{universe}/{node}` | mTLS coordinated subscription; `-listen` |
| 8444 | `POST /v3/enroll` | Server-authenticated HTTPS, Pod-bound bearer token; `-enroll-listen` |
| 8445 | `GET /v3/replica-proof` | Server-authenticated HTTPS on every controller Pod, probed directly by the leader |
| 8446 | `POST /v3/proof` | Fresh mTLS handshake proving installed dataplane trust |
| 8081 | `/healthz`, `/readyz` | Controller HTTP probes; `-health-listen` |
| 9443 | Peer RPCs | Dedicated dataplane mTLS listener |
| 9090 | `/startupz`, `/readyz`, `/livez`, `/status`, `/metrics` | Dataplane HTTP management |

The managed controller Service exposes 8443, 8444, and 8446. Port 8445 is
Pod-to-Pod, and 8081 is a Pod probe port. Volume listeners and origin fetches
continue to use ordinary HTTP.

ConfigMaps persist immutable 512 KiB chunks with a SHA-256-verified commit pointer
updated using resourceVersion compare-and-swap. Generations publish only after
commit. Revisions, slot ownership, port reservations, and rollout decisions
survive controller restarts. Retain the state namespace and CA state
across restarts. Do not reset state while dataplanes depend on its revision and
placement continuity. The renamed metadata is a clean import, not an in-place
migration of existing `racer.io` state.

## mTLS enrollment and coordinated subscription

This is a breaking protocol cutover: deploy matching controller and dataplane
versions. Legacy subscription paths, detached signatures, and shared peer-key
bundles are not supported.

Each dataplane generates its private key locally. `POST /v3/enroll` on port 8444
accepts server-authenticated HTTPS with:

```text
Authorization: Bearer <pod-bound-token>
X-Racer-Boot: <64-hex-process-boot-nonce>
Content-Type: application/json

{"csr":"<PEM CSR>","pod_namespace":"<namespace>","pod_name":"<name>"}
```

The response is JSON containing `certificate` (PEM leaf plus issuing root),
`generation` (trust generation), and `issuer` (SHA-256 of the issuing root DER).
Unknown request fields are rejected. The server derives identity from TokenReview,
live Pod/DaemonSet/Site ownership, Node membership, and committed topology, not
CSR subjects or SANs. The token's audience is `racer-control`; it is used for
issuance and renewal, not subscription heartbeats. Positive TokenReview results
are cached for at most five seconds, capped by token expiry. The independent
review client uses `-token-review-qps=20` and `-token-review-burst=30` by default.

Node leaves identify `spiffe://racer/universe/<universe>/node/<node>/pod/<podUID>`.
Node identity persists across Pod replacement, but each Pod and process has its
own credentials; PKI membership is tracked by Pod UID and boot nonce. An existing
draining process can renew its retained identity after exclusion or Site disable;
a new process cannot enroll through that retained-identity path. Controller
replicas generate local keys and exchange CSRs/certificates through Pod-owned
`racer-replica-<podUID>` ConfigMaps. Their leaves identify
`spiffe://racer/controlplane` and the controller Service DNS name.

The production subscription uses TLS 1.3 with a verified client certificate:

```text
GET /v3/<universe-hex>/<node-hex>
Accept: application/x-protobuf
X-Racer-Boot: <64-hex-process-boot-nonce>
X-Racer-Profile: 1
Prefer: wait=0
X-Racer-Trust-Generation: <installed-generation>
X-Racer-Trust-Digest: <installed-bundle-digest>
X-Racer-Certificate-Issuer: <verified-leaf-root-digest>
X-Racer-Old-Connections: <old-context-connection-count>
```

Subsequent requests include worker feedback in `X-Racer-Phase` and
`X-Racer-Digest`. `X-Racer-Needs-Config: 1` requests the snapshot again, and
`X-Racer-Forward-Eligible` participates in forward recovery. Successful responses
are always 200 with protobuf `ControlCommand` messages and
`Content-Type: application/x-protobuf`; configuration is included when needed.
The `/v3` handler does not use ETags or return 304.
TLS authentication failures terminate the handshake. After TLS authentication,
both path identities must be 64 hex characters. Malformed requests return 400;
unknown universes return 404; invalid credentials or unselected Pod identities
return 403; a competing live boot incarnation returns 409; transient API,
durability, or local overload failures return 503. Legacy paths are not registered.

Configure the dataplane using the bootstrap exports:

```sh
export RACER_CONTROL_SERVER_NAME=racer-controlplane.racer-system.svc
export RACER_CONTROL_PLANE_URL="https://$RACER_CONTROL_SERVER_NAME:8443/v3/$RACER_UNIVERSE/$RACER_NODE"
export RACER_ENROLL_URL="https://$RACER_CONTROL_SERVER_NAME:8444/v3/enroll"
export RACER_TLS_TRUST_DIR=/var/run/racer-trust
export RACER_CONTROL_TOKEN_FILE=/var/run/racer-control/token
```

Supply `RACER_POD_NAMESPACE`, `RACER_POD_NAME`, and `RACER_POD_UID` through the
Downward API. Replace `racer-system` with the deployment namespace. The trust-proof
URL defaults to the enrollment host on port 8446 with path `/v3/proof`;
`RACER_TRUST_PROOF_URL` overrides it. Every heartbeat validates the enrolled
certificate, Pod UID, and boot membership, including retained removal recipients.

Commands bind node, Pod UID, process boot nonce, revision, exact snapshot digest, and
resource profile. Durable phases prepare, enable reception, activate ingress,
then retire. Abort is possible only before reception. All targets must report
prepared and receive-ready before normal activation. A controller restart
recollects acknowledgments. Missing Pods permit abort or forward recovery;
network timeout alone does not remove a participant. Partitions retain the last
serving configuration while credentials remain valid. Peer requests require the
certificate's universe, node, and selected Pod UID to match the addressed routing
generation. Snapshot admission enforces a conservative per-recipient wire
budget of 64 MiB minus 1 KiB.

### Independent storage policy

The storage controller watches Nodes and Sites separately from topology. It uses
canonical Site membership and resolves capacity through
`internal/racer.ResolveCacheSize`: Node annotation, then Site default, then 10GiB.
Invalid input retains the previous desired bytes/version and records a validation
error in the storage record. It does not block topology reconciliation.

Set `spec.components.racer.cacheSize` on the Site or annotate a Node with
`racer.unbounded-cloud.io/cache-size`. Remove the Node annotation to restore live
Site inheritance; patch the Site field to `null` to restore the 10GiB default.
An empty annotation is invalid. Quantities must be whole bytes, at least 32MiB,
and round up to 4MiB. Capacity includes slab index/layout overhead, not just
payload. API normalization allows aligned signed 64-bit file offsets; the runtime
currently rejects automatic layouts above 4TiB or below 32MiB per existing worker.
These runtime failures retain the actual old capacity and are reported separately
from invalid input. See the [public guide](../../docs/content/guides/racer.md#set-cache-capacity)
for commands and operating requirements.

The managed workload has fixed 3 CPU/4GiB requests and limits. Capacity changes
do not alter the Pod template or topology revision. Growth and shrink flush the
cache asynchronously using a fresh-inode transaction and bounded admission
fencing; they do not restart workers, pools, or RDMA registrations. Persisted
slab geometry is authoritative on restart; creation environment does not resize
an existing slab.

Each Node UID has a separate `racer-storage-<node-identity>` ConfigMap in the
state namespace, labeled `racer.unbounded-cloud.io/state: storage`. Its random
32-byte identity and monotonic version survive controller restarts. Only an
effective byte change advances the version. ResourceVersion CAS commits precede
publication; no storage persistence occurs on heartbeats. Preserve these records
with the other controller state. A running dataplane rejects a changed policy
identity, lower version, or same-version byte change rather than accepting reset
state as a new resize authority.

Profile 1 is unchanged. Clients advertise `X-Racer-Storage-Policy: 1` to receive
the optional `ControlCommand.storage_policy` over the mTLS subscription, with
Node/universe/Pod/process binding checked before policy admission. The field is
sent on config-free heartbeats too. Clients without this capability receive normal
topology commands and an `unsupported` Node status observation. Storage versions are unrelated to snapshot
revisions, topology epochs, and rollout phases.

Feedback uses `X-Racer-Storage-Identity`, `X-Racer-Storage-Version`,
`X-Racer-Storage-State` (`pending`, `applied`, or `failed`), and
`X-Racer-Storage-Applied-Bytes`. Optional `X-Racer-Storage-Shards` reports actual
geometry; `X-Racer-Storage-Error` is a hex-encoded diagnostic, capped at 1024 decoded
bytes, accepted only for a bound failed report. Applied requires the exact desired byte count.
The controller accepts feedback only for the current policy previously offered
to the authenticated Node/Pod/boot tuple. Observations are memory-only; controller
restart requires a fresh offer and acknowledgment, and Pod/process replacement
clears prior applied observations. Invalid or stale feedback does not fail the
topology heartbeat. Repeated equal policies preserve runtime outcome; newer
versions coalesce to the latest desired request.

Rust `Updates::desired_storage`, `report_storage`, and `storage_policy_status`
provide a thread-safe runtime integration boundary. Receipt records `Pending`,
never `Applied`; this delivery layer does not mutate slab capacity. The runtime
coordinator reports the actual installed capacity and shards independently of
topology readiness and errors.

The controller owns Node annotation `racer.unbounded-cloud.io/cache-status`, a
JSON observation with `source` (`node`, `site`, `default`), `requested` quantity,
nullable unrounded `requestedBytes`, last-good normalized `effectiveBytes`,
`policyIdentity`, `policyVersion`, `phase`, `policyPhase`, `validationError`,
`error`, `appliedBytes`, `appliedVersion`, `shards`, `selectedPodUID`, `boot`,
`fresh`, `lastSeen`, and `updatedAt`. Invalid desired input sets `phase: invalid`
while `policyPhase` still describes the last-good policy. A fresh old client
reports `unsupported`. Missing observations report `pending`; observations older
than 15 seconds report `stale` and expose zero applied bytes/version/shards.
Pod/boot replacement and controller restart require a new offer/ack pair.

Node reconciliation polls at five seconds. Semantic transitions publish on the
next reconciliation; unchanged fresh reports refresh timestamps at most once per
minute. `lastSeen` is therefore a coalesced observation, and consumers should use
`updatedAt` to detect a stopped controller. Unchanged stale observations do not
rewrite. ResourceVersion-guarded patches cannot overwrite concurrent Node edits.
Neither topology nor storage input predicates consume this output annotation.
Equivalent quantities and source changes update status without advancing policy
version or resetting the cache. Requested text is capped at 256 bytes and errors
at 1024 bytes; identities and errors never become metric labels.

## Durable CA state and hot reload

The fenced leader owns `racer-ca` (Secret, `state.json`) and `racer-trust`
(ConfigMap, `bundle.json`). The Secret holds CA private keys, rotation state,
issuance expiry watermarks, and references to immutable participant ConfigMap
shards. Preserve the complete state namespace across restarts. Missing private
state alongside existing public trust or topology fails closed instead of
silently creating a new CA. Never delete or edit CA state to request rotation.

The public bundle contains `version: 1`, monotonic `generation`, `active` (the
lowercase SHA-256 digest of the active root DER), and `certificates` (PEM roots).
Only public trust is projected to dataplanes; no fleet-wide private key is shared.

Example trust volume and container mount fragments:

```yaml
# Pod spec.volumes:
- name: trust
  configMap:
    name: racer-trust
    items:
      - key: bundle.json
        path: bundle.json
# Container volumeMounts:
- name: trust
  mountPath: /var/run/racer-trust
  readOnly: true
```

Mount the directory without `subPath`. The dataplane reloads trust independently
of subscription requests, installs overlap roots before renewal, and renews its
leaf when the issuer changes or its jittered renewal time arrives. Every worker
must install the new context before trust is acknowledged. Invalid bundles,
rollback, or same-generation divergence retain the last valid context and report
an error; that does not extend certificate validity. New connections use the
installed context while old connections drain. Controllers also hot-reload their
production and proof contexts, including on standbys.

### CA rotation

The serving binary defaults to `-ca-rotation-interval=720h` (30 days). Leaves
default to 24 hours and the retirement clock-skew allowance is five minutes.
Rotation advances the public generation at each transition:

1. **Stable:** one trusted root issues production leaves.
2. **Overlap:** persist and publish the next root alongside the old root. Production
   issuance stays on the old root. Proof listeners serve a next-root certificate.
   Every retained dataplane and controller process must acknowledge the exact
   bundle and prove it through a fresh TLS handshake before issuance switches.
3. **Switched:** the new root issues production leaves while both roots remain
   trusted. All retained processes must provide fresh proofs and report old
   connections drained. Retirement also waits past the old root's latest issued
   leaf expiry plus clock skew.
4. **Stable again:** remove the old root and publish the single-root bundle.

`POST /v3/proof` on 8446 has an empty body and the boot/trust/issuer/connection
headers shown above. It returns 204 on success. Each exchange uses a new mTLS
connection; claimed headers alone cannot satisfy the proof barrier. During
overlap an old-root client leaf can prove trust in the next-root server leaf.
The leader probes each replica's `GET /v3/replica-proof` on 8445 using a fresh
server-authenticated TLS connection pinned to that replica's CSR/key and boot.
Replica issuance itself uses Kubernetes ConfigMaps, not an HTTP enrollment route.
The replica-proof response is JSON with `pod_uid`, `boot_id`, `csr_digest`,
`generation`, `digest`, and `old_connections_drained`; it describes installed
contexts, not merely certificates delivered through Kubernetes.

Loss of readiness, labels, or contact is not proof that a process has stopped.
CA participants retire after authoritative Pod absence or proven container
replacement. Leader takeover fences mutations and requires fresh proof evidence.
An unavailable retained participant can therefore hold rotation at a barrier.

Request an operational rotation by annotating the public ConfigMap with a unique
nonempty nonce. Use the actual state namespace, and let a current rotation finish
before requesting another:

```sh
kubectl -n unbounded-system annotate configmap racer-trust \
  racer.unbounded-cloud.io/rotate-ca="$(date -u +%Y%m%dT%H%M%S%N)" --overwrite
kubectl -n unbounded-system get configmap racer-trust -o jsonpath='{.data.bundle\.json}'
```

Repeating the current request nonce is idempotent. Observe bundle generation, active root,
and root count, then compare dataplane `/status` TLS generation, trust digest,
issuer, installed-worker counts, and error. A switched two-root bundle is expected
until old leaves expire; an annotation does not bypass proofs or expiry. Do not
patch private state, shorten persisted expiry watermarks, or remove participants
to force the barrier.

### Native TLS requirements

Dataplane builds require `cc`, `ar`, `pkg-config`, libibverbs headers, and OpenSSL
3 headers/libraries (Ubuntu: `build-essential libibverbs-dev libssl-dev pkg-config`).
The current kTLS eligibility gate requires **OpenSSL >= 3.5 and Linux >= 6.14**
for TLS 1.3 rekeying. OpenSSL 3.0 and older kernels use encrypted software TLS for
the entire connection. Meeting the version gate does not guarantee offload;
inspect `racer_dataplane_tls_ktls_tx_connections_total` and
`racer_dataplane_tls_ktls_rx_connections_total` separately. TLS file sends use
`SSL_sendfile` only with TX offload and buffered encrypted writes otherwise.

Implementation references (paths from the repository root):
`cmd/racer-controlplane/enrollment.go:77-214`,
`cmd/racer-controlplane/tls_server.go:133-211`,
`cmd/racer-controlplane/trust_proof.go:59-130`,
`cmd/racer-controlplane/replica_tls.go:422-530`,
`internal/racer/pki/manager.go:535-651`,
`cmd/racer-dataplane/src/credentials.rs:378-517`, and
`cmd/racer-dataplane/src/tls_native.c:39-65`.

## Checks

From the repository root:

```sh
GOTOOLCHAIN=go1.26.6 go test -mod=readonly ./cmd/racer-controlplane/... ./internal/racer/... ./internal/operator/components/racer/...
GOTOOLCHAIN=go1.26.6 go test -mod=readonly -race ./cmd/racer-controlplane/... ./internal/racer/... ./internal/operator/components/racer/...
GOTOOLCHAIN=go1.26.6 golangci-lint run ./cmd/racer-controlplane/... ./internal/racer/... ./internal/operator/components/racer/...
```

`envtest_test.go` and the operator's `admission_test.go` and
`reconcile_api_test.go` skip API-server tests
unless `KUBEBUILDER_ASSETS` points to compatible kube-apiserver and etcd binaries.
They exercise real CAS, operator workload admission, Site/bootstrap/affinity
agreement, informer-driven reconciliation, and Service patches.
`TestAPIDefaultedResourcesAreNoOp` verifies that API-defaulted resources cause
zero steady-state SSA writes and that live workload drift triggers one repair.
Envtest does not
run a scheduler; affinity matching is checked against persisted Node metadata.

Install matching API-server assets into the repository and run the integration
tests explicitly:

```sh
mkdir -p bin tmp
GOTOOLCHAIN=go1.26.6 GOBIN="$PWD/bin" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37.0 --bin-dir "$PWD/bin/envtest" -p path)"
GOTOOLCHAIN=go1.26.6 TMPDIR="$PWD/tmp" go test -mod=readonly ./cmd/racer-controlplane ./internal/operator/components/racer \
  -run '^(TestB14RealAPICAS|TestControllerAPIIntegration|TestSiteWorkloadAdmission|TestAPIDefaultedResourcesAreNoOp)$' -count=1 -v
```

The Racer CI job installs Kubernetes 1.37.0 assets, exports `KUBEBUILDER_ASSETS`,
and explicitly runs these controller and operator tests before the Rust suites.

`coordination_harness_test.go` preserves the production Go/Rust harness. Set
`RACER_COORDINATION_TEST_BIN` to the absolute path of the imported dataplane's
prebuilt Rust lib-test executable, then run:

```sh
GOTOOLCHAIN=go1.26.6 go test -mod=readonly ./cmd/racer-controlplane \
  -run '^TestB(13ProductionCatchup|14ProductionCoordination|15ProductionForward|15ProductionMultiRecipient|16ProductionHeartbeatTLS)$' \
  -count=1 -v
```

These tests invoke ignored Rust children `coordination_tests::production_coordination_child`,
`coordination_tests::production_catchup_child`,
`coordination_tests::production_forward_child`, and
`forward_multi_tests::production_multi_forward_child`. Keep these entry points
available when moving the dataplane. These fixtures supply test TLS identities.

`TestStorageRuntimeTLSResizeRestart`, enabled by `RACER_DATAPLANE_BINARY`,
runs the actual daemon against the Go mTLS subscription handler using fixture-issued
certificates. It verifies grow/shrink across shard counts, actual inode replacement,
process-local and Node status, above-envelope failure, last-good input errors,
equivalent-size no-ops, topology
independence, and controller/daemon restart with persisted capacity authoritative
before control reconnects. Kubernetes is fake; this fixture does not exercise
production enrollment or CA rotation (`storage_runtime_test.go:31-110`).

`make racer-crosslang-test` sets both `RACER_COORDINATION_TEST_BIN` and
`RACER_DATAPLANE_BINARY`. The latter enables `TestProductionCARotationTraffic`,
which launches two real Rust daemons with production enrollment/proof handlers,
fake Kubernetes/TokenReview, short-lived leaves, and continuous Go SDK reads.
Its assertions cover overlap, issuer switch, old-root retirement, renewed worker
contexts, peer traffic, and TLS reconnects. This is not a live Kubernetes test.
Use workspace-local ext4 `TMPDIR`, sufficient physical cores and locked memory,
and an external timeout. Without the binary variable the campaign skips.

Shipping resources and their trust projections, management-probe,
deployment-profile, and Site scheduling contracts are tested in
`internal/operator/components/racer`.
