<!-- Copyright (c) Microsoft Corporation. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# RACER control plane

Kubernetes topology controller and authenticated protobuf control server for the
RACER dataplane. Imported from `racer` commit
`c9bf09848a66df58d7cde6bb09bd5c6fcc61913c` for Phase 1 integration.

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

Phase 1 retains Node/Pod/Service membership. A Node's
`racer.unbounded-cloud.io/universe` annotation chooses its universe, defaulting
to `default`. Site-based identity and operator-managed membership are Phase 2 work.

A volume is a selector-based Service with the following annotation prefix:

| Annotation (`racer.unbounded-cloud.io/`) | Default | Meaning |
| --- | --- | --- |
| `origin-service` | required | Separate origin Service name |
| `origin-namespace` | volume namespace | Origin Service namespace |
| `origin-port` | required | TCP Service port name or number, not targetPort |
| `universe` | `default` | Universe name |
| `slot-count` | `131072` | Fixed slots, 1-262144; immutable per volume identity |
| `listener-port` | allocated | Optional immutable port, 1024-65535 |
| `cache-generation` | `1` | Unsigned 64-bit dataset/cache generation |
| `routing-algorithm` | `2` | Only canonical destination-rooted routing is supported |
| `max-candidate-attempts` | `3` | Bounded fallback attempts, 1-8 |

Volume Service selectors must include both labels:

```yaml
racer.unbounded-cloud.io/dataplane: "true"
racer.unbounded-cloud.io/universe: default
```

The controller watches labeled dataplane Pods. Participants need a Ready Node
and a selected, DaemonSet-controlled, Running Pod with an IP. Pod readiness is
not required for initial configuration. Selected Pods must belong to the volume
Service's namespace and their Nodes must belong to its universe. Kubernetes
EndpointSlices independently exclude unready Pods from client traffic.

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
deletion. Port 9090 is always reserved for management. Supply
`-reserved-management-ports=9090,10000` if a dataplane uses an additional
management port. Include old and new ports during a rollout. Existing allocations
cannot move; a conflicting volume needs a new Service identity with a safe port.

The Node's `racer.unbounded-cloud.io/fabric` annotation is copied into snapshots
for dataplane RDMA eligibility. Omit it for HTTP-only operation. A matching fabric
name alone does not validate hardware connectivity.

## Bootstrap and controller lifecycle

Bootstrap uses the controller binary, a Node name, a universe scheduling mirror,
and the Pod's primary IP. For example, inside the bootstrap container:

```sh
POD_IP="$POD_IP" racer-controlplane \
  -bootstrap-node="$NODE_NAME" -bootstrap-universe="$POD_UNIVERSE" \
  -bootstrap-namespace=racer-system -bootstrap-service=racer-controlplane \
  -bootstrap-port=8080
```

This prints shell exports for `RACER_UNIVERSE`, `RACER_NODE`, and
`RACER_CONTROL_ADDRESS`. It checks that the Node annotation and universe label
match the expected universe, reads the controller Service, and selects a numeric
ClusterIP endpoint matching `POD_IP`. Recreating that Service with a different
ClusterIP requires restarting dataplane Pods.

Phase 1 identities are lowercase SHA-256 hex digests of these exact byte strings:

```text
universe: "racer/universe/v1\x00" || universe_name
node:     "racer/node/v1\x00"     || kubernetes_node_uid
```

Here `\x00` is one NUL byte. Kubernetes metadata uses the new Unbounded prefix;
cryptographic domains remain unchanged. Pod replacement retains Node identity.
Node deletion/recreation changes identity and leaves the old recipient's removal
history. Universe changes require restarting and relabeling dataplane Pods.

Only the elected leader listens on the subscription port (default 8080).
`/readyz` on that port selects the serving leader. Standbys keep informer caches
but do not serve subscriptions. `/healthz` uses `-health-listen` (default 8081).
The default leader-election and durable-state namespace is `racer-system`.

ConfigMaps persist immutable 512 KiB chunks with a SHA-256-verified commit pointer
updated using resourceVersion compare-and-swap. Generations publish only after
commit. Revisions, slot ownership, port reservations, and rollout decisions
survive controller restarts. Retain the state namespace and signing Secrets
across restarts. Do not reset state while dataplanes depend on its revision and
placement continuity. The renamed metadata is a clean import, not an in-place
migration of existing `racer.io` state.

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

## Checks and integration handoff

From the repository root:

```sh
GOTOOLCHAIN=go1.26.6 go test -mod=readonly ./cmd/racer-controlplane/...
GOTOOLCHAIN=go1.26.6 go test -mod=readonly -race ./cmd/racer-controlplane/...
GOTOOLCHAIN=go1.26.6 go vet -mod=readonly ./cmd/racer-controlplane/...
```

`envtest_test.go` skips its API-server tests unless `KUBEBUILDER_ASSETS` points
to compatible kube-apiserver and etcd binaries. The tests exercise real CAS,
admission validation, informer-driven reconciliation, and Service patches.

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

Temporary YAML files in `testdata/` preserve imported test assertions; they are
not shipping manifests. Phase 2 must move these contracts to tests of actual
operator constructors before removing the fixtures:

- `TestShippingSigning`: managed Secret RBAC, controller service account and
  no key mounts, signed transport, bundle-only read-only dataplane projections.
- `TestManagementBindingMatchesPodIPProbes`: downward-API primary Pod IP and
  management readiness/liveness probes on port 9090.
- `TestShippingDataplaneProfile`: scheduling/universe mirror, security contexts,
  resource profile, startup/readiness/liveness and shutdown deadlines, fail-closed
  memlock/preflight/exec ordering, guarded bootstrap, and cache hostPath.
- `TestProvisionedNodeAdmission`: real API validation of constructed DaemonSets
  and universe-mirror admission policy, including update rejection.
- `TestBootstrapUniverseSchedulingMirror`: retain the bootstrap mismatch and
  missing-identity checks when Phase 2 replaces Node-based membership with Sites.

The separate e2e import must own source `racer-controlplane/e2e/` fixtures,
deployment/vLLM scenarios, images, and deployment construction. Update its
metadata prefix, root-module imports/build contexts, bundle projections, and
legacy HTTP expectations to the `/v2`-only protocol. Ordinary tests here do not
import e2e fixture packages. Keep the coordination harness and envtest checks
wired into integration validation after the operator migration.
