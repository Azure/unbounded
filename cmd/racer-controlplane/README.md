<!-- Copyright (c) Microsoft Corporation. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# RACER control plane

Kubernetes topology controller and authenticated protobuf control server for the
RACER dataplane, with Site-derived membership and signed `/v2` subscriptions.

Build and run from the Unbounded repository root with Go 1.26.6 and Kubernetes
credentials (in-cluster or `KUBECONFIG`):

```sh
GOTOOLCHAIN=go1.26.6 go build -mod=readonly -o bin/racer-controlplane ./cmd/racer-controlplane
GOTOOLCHAIN=go1.26.6 go run -mod=readonly ./cmd/racer-controlplane \
  -listen :8080 -state-namespace racer-system
```

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

A cache is a cluster-scoped `P2PCache`. Its selector matches labels on **Site
objects**, and only matched Sites with Racer enabled participate:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: P2PCache
metadata:
  name: dataset
spec:
  siteSelector:
    matchLabels:
      environment: production
  cacheGeneration: 1
  maxCandidateAttempts: 3
```

An omitted or empty selector matches all Sites without enabling Racer on them.
The name must be a DNS label of at most 63 characters. `cacheGeneration` defaults
to 1, is nonnegative, and cannot decrease. `maxCandidateAttempts` defaults to 3
and accepts 1-8. Every cache has 262144 slots and canonical destination-rooted
routing. Each selected Site retains its own independent universe and topology.
Cache identity includes the resource UID and cache generation within that universe;
recreating a resource creates a new identity.

Clients use HTTP over `/dev/racer/<name>/cache`. Every participant node must run
an origin serving the same logical dataset over `/dev/racer/<name>/origin`.
The origin process owns that socket. Racer creates the cache socket and its
parent directory. Mount directories, never individual socket files: Racer mounts
the entire `/dev/racer` host directory, and applications mount their cache's
parent directory. Managed directories use group 65532 and mode 2770; sockets use
mode 0660. Nonroot applications need that supplemental group. A manually managed
deployment can set `-socket-root` on the controller; all mounts must agree with
that absolute root, and even 63-character cache names must fit the 107-byte Unix
path limit. Peer traffic uses
separate authenticated TCP listeners. Plain client HTTP is admitted only on UDS.

Participants need an eligible Ready Linux Node and a DaemonSet-controlled, Running
Pod with an IP, the dataplane/universe labels, and the `racer-dataplane` service
account in the controller's namespace. Pod readiness is not required for initial
configuration. With no selected caches, managed Pods receive signed `idle`
snapshots and remain available for later cache creation. Historical excluded,
moved, or unavailable recipients retain removal snapshots with `idle` unset.

The status subresource reports `observedGeneration`, `Accepted`, `Ready`, and
`participants.desired/ready`. Desired counts include eligible starting Nodes with
missing or unready Pods. Ready requires at least one desired participant, current
activation acknowledgments from healthy workers across all selected Sites, and
safe retirement after selector withdrawal. It does not test origin availability.
No matching Sites or no participants yields Accepted true and Ready false.

```sh
kubectl wait p2pcache/dataset --for=condition=Ready --timeout=120s
```

After editing a cache, first wait for `status.observedGeneration` to equal the new
`metadata.generation`, then wait for Ready, to avoid observing an old True condition.

Automatic peer listener allocation uses 10000-29999. Reservations persist after
deletion. Port 9090 is always reserved for management. Supply
`-reserved-management-ports=9090,10000` if a dataplane uses an additional
management port. Include old and new ports during a rollout. Existing allocations
cannot move; resolve management-port conflicts before reconciling that Site.

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
  -bootstrap-port=8080
```

This prints shell exports for `RACER_UNIVERSE`, `RACER_NODE`, and
`RACER_CONTROL_ADDRESS`. It requires a Node UID, rejects excluded, deleting, or
unassigned Nodes, and checks the Site-derived universe against the required
`-bootstrap-universe` value. It reads the controller Service and selects a numeric
ClusterIP endpoint matching `POD_IP`. Recreating that Service with a different
ClusterIP requires restarting dataplane Pods.

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
fallback. Within format-3 generations, UID-only members retain
authority until an observed Pod with that exact UID supplies its namespace/name.
Legacy recipients never observed again remain retained, including their history;
absence from the cache cannot safely migrate or release them.
Node replacement similarly retains the old identity as a tombstone and does not
adopt its still-running Pod into the new Node identity.

The dataplane slab has a process-lifetime exclusive `flock`. During replacement,
the old process must release the slab before the replacement can open it. Preserve
the existing slab path and normal process teardown; deleting or replacing the slab
file to bypass contention would defeat that lifecycle protection. Disabling a
Site's Racer deployment removes its dataplane; shared control-plane state and
signing keys are retained for removal delivery and subsequent re-enrollment.

Only the elected leader listens on the subscription port (default 8080).
`/readyz` on that port selects the serving leader. Standbys keep informer caches
but do not serve subscriptions. `/healthz` uses `-health-listen` (default 8081).
The default leader-election and durable-state namespace is `racer-system`.

ConfigMaps persist immutable 512 KiB chunks with a SHA-256-verified commit pointer
updated using resourceVersion compare-and-swap. Generations publish only after
commit. Revisions, slot ownership, port reservations, and rollout decisions
survive controller restarts. Retain the state namespace and signing Secrets
across restarts. Do not reset state while dataplanes depend on its revision and
placement continuity. This API requires format-3 durable generations. Earlier
formats are rejected explicitly; there is no annotated-Service compatibility or
automatic durable-state migration.

## Signed coordinated subscription

The production HTTP protocol is exclusively:

```text
GET /v2/<universe-hex>/<node-hex>
Accept: application/x-protobuf
Authorization: Bearer <pod-bound-token>
X-Racer-Boot: <64-hex-process-boot-nonce>
X-Racer-Profile: 1
Prefer: wait=0
```

Subsequent requests include worker feedback in `X-Racer-Phase` and
`X-Racer-Digest`. `X-Racer-Needs-Config: 1` requests the snapshot again, and
`X-Racer-Forward-Eligible` participates in forward recovery. Successful responses
are always 200 with protobuf `SignedControlCommand` envelopes and
`Content-Type: application/x-protobuf`; exact signed snapshot bytes are included
when configuration is needed. The `/v2` handler does not use ETags or return 304.
Both path identities are 64 hex characters. Malformed requests return 400;
unknown universes return 404; invalid credentials or unselected Pod identities
return 403; a competing live boot incarnation returns 409; transient API,
durability, or local overload failures return 503. Legacy paths are not registered.

Configure the dataplane using the bootstrap exports:

```sh
export RACER_CONTROL_PLANE_URL="http://$RACER_CONTROL_ADDRESS/v2/$RACER_UNIVERSE/$RACER_NODE"
export RACER_PEER_KEYS_DIR=/var/run/racer-peer-signing
export RACER_CONFIG_KEYS_DIR=/var/run/racer-config-verify
export RACER_CONTROL_TOKEN_FILE=/var/run/racer-control/token
```

The token must be a projected Pod-bound service-account token with audience
`racer-control`. Every heartbeat validates the selected Pod UID, including after
retirement. Positive TokenReview results are cached for at most five seconds
from review initiation, capped by token expiry; failures are not cached.
`-token-review-qps=20` and `-token-review-burst=30` configure the independent
review client. Its cache holds at most 4096 positive entries, with 64 distinct
in-flight reviews and 64 followers per flight; reviews have one-second deadlines.
Signatures authenticate commands but do not encrypt bearer tokens. Use a trusted
cluster network or an authenticated encrypted transport proxy.

Commands bind node, process boot nonce, revision, exact snapshot digest, and
resource profile. Durable phases prepare, enable reception, activate ingress,
then retire. Abort is possible only before reception. All targets must report
prepared and receive-ready before normal activation. A controller restart
recollects acknowledgments. Missing Pods permit abort or forward recovery;
network timeout alone does not remove a participant. Partitions retain the last
serving configuration; boot nonces do not fence an isolated process's peer
credentials. Snapshot admission enforces a conservative per-recipient wire
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
the optional `ControlCommand.storage_policy`, covered by the existing command
signature and Node/universe/Pod/process binding. The field is sent on config-free
heartbeats too. Older clients receive normal topology commands and an
`unsupported` Node status observation. Storage versions are unrelated to snapshot
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

## Managed signing keys

At startup every replica creates or reuses `racer-config-signing` and
`racer-peer-signing` in the state namespace. Existing malformed key material
fails startup. The controller reads keys through the Kubernetes API, without key
mounts. `RACER_SIGNING_KEY`, including an empty value, is rejected.

Each Secret contains controller-private `ring.json` and consumer `bundle.json`.
Project **only `bundle.json`** to dataplanes. The configuration bundle contains
`version`, `generation`, `active` public key, and a `public` trust array. The peer
bundle additionally carries the active `seed`. Keys are lowercase hex-encoded
32-byte Ed25519 material. Configuration signing seeds never reach dataplanes.

Example config-verifier volume and container mount fragments:

```yaml
# Pod spec.volumes:
- name: config-verify
  secret:
    secretName: racer-config-signing
    items:
      - key: bundle.json
        path: bundle.json
# Container volumeMounts:
- name: config-verify
  mountPath: /var/run/racer-config-verify
  readOnly: true
```

Mount directories without `subPath` so projected bundles can reload. Apply the
same pattern to `racer-peer-signing` at `RACER_PEER_KEYS_DIR`. Do not project
`ring.json` or use old raw-public-key examples. `-generate-key DIR` still creates
raw seed/public files for standalone fixtures; they must be hex-encoded into
the bundle format before dataplane use.

The state-namespace Role needs name-restricted `get`/`update` on the two signing
Secrets and namespace-wide `create`/`list`/`watch`. The cache watches all Secrets
in that namespace, with event processing restricted to the signing names.
All replicas observe active-key changes. Only the leader rotates keys, using
`-signing-rotation-interval=24h` and `-signing-propagation-delay=10m` by default.
The delay must be positive and shorter than the interval. Pending public keys
are published before activation; pending peer seeds are never projected.
Generation, active key, and bundle are updated atomically. Invalid runtime
updates retain the last valid signer. Missing Secrets are recreated only at the
next startup, so preserve them to retain identity.

Config signatures use domain `racer/config/v2`; command signatures use
`racer/control/v1`; key IDs use `racer/public-key/v2`. These are protocol domains,
not Kubernetes metadata names, and remain compatible with the imported dataplane.

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
  -run '^TestB(13ProductionCatchup|14ProductionCoordination|15ProductionForward|15ProductionMultiRecipient|16ProductionHeartbeatTokenReload)$' \
  -count=1 -v
```

These tests invoke ignored Rust children `coordination_tests::production_coordination_child`,
`coordination_tests::production_catchup_child`,
`coordination_tests::production_forward_child`, and
`forward_multi_tests::production_multi_forward_child`. Keep these entry points
available when moving the dataplane. TokenReview is simulated in this harness.

`TestStorageRuntimeSignedResizeRestart`, enabled by `RACER_DATAPLANE_BINARY`,
runs the actual daemon against the signed Go handler. It verifies grow/shrink
across shard counts, actual inode replacement, process-local and Node status,
above-envelope failure, last-good input errors, equivalent-size no-ops, topology
independence, and controller/daemon restart with persisted capacity authoritative
before control reconnects. `make racer-crosslang-test` enables both harnesses.

Shipping resources and their signing, management-probe, deployment-profile, and
Site scheduling contracts are tested in `internal/operator/components/racer`.
