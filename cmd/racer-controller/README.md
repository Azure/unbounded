# Racer controller implementation status

Phases 1-7 implement and verify the server: configuration, bounded codecs, membership/catalog calculation,
one-shot initialization, durable publication CAS, issuer/shared-key rotation, and
certificate issuance, TokenReview bootstrap, and operational leader-scoped HTTPS/mTLS
serving. The operator owns both Racer workloads. Phase 7 exercises real Kubernetes
API-server persistence, manager election/failover, TLS enrollment, and publication scale.
The `initialize` command performs Kubernetes writes; normal startup validates
existing durable state. Constructors only wire dependencies; they open no files
or listeners and start no goroutines. HTTP readiness requires synchronized inputs,
usable credentials, a committed publication, and an accepting TLS listener.

The [control API](../racer-dataplane/CONTROL_API.md) is shared with the Rust
dataplane. The [design](../../designs/racer-control-plane.md) describes intended
behavior, recovery protocols, and measured constraints. Rust client runtime,
identity-file handling, and dataplane transport were implemented independently
and are outside the scope of this server work. Shared contract tests exercise
both the test-only reference codec and the existing Rust runtime codec.

## Structure

- `api/racer/v1alpha1`: cluster-scoped ClusterCache resource and generated CRD.
- `internal/racer`: two ordinary controller-runtime reconcilers, manager,
  publication state, token bootstrap, certificate issuance, and mTLS server.
- `internal/racer/wire`: shared contract declarations and bounded codec boundaries.
- `deploy/racer`: controller/RBAC/config/admission templates.
- `internal/operator/components/racer`: the sole controller Deployment and dataplane
  DaemonSet owner, reusing the pure `internal/racer.DesiredDaemonSet(WorkloadConfig)` builder.

`WorkloadConfig` validates only dataplane deployment inputs: cluster UUID,
namespace, peer port, DaemonSet/service account/trust names, HTTPS control
URL, and image. The operator loads it with `WorkloadConfigFromLookup` from the
preserved `racer-config` payload. Controller `Config` and `ConfigFromLookup` cover
runtime settings independently; limits, rotation policy, serving TLS, and durable
state settings are not prerequisites for building a DaemonSet. Invalid controller
runtime settings are rejected by controller startup rather than workload planning.
`RACER_DATAPLANE_IMAGE`, `RACER_CONTROL_URL`, and `RACER_BOOTSTRAP_TRUST_CONFIGMAP`
remain deployment wiring in `racer-config`, but the controller does not read them.

Shared cache keys and peer trust roots are delivered through `GET /v1/keyring`,
not a dataplane Secret mount. The existing bounded 512 KiB JSON bundle is served
with an independent generation long poll (no cursor: immediate 200; newer: 200;
equal: up to 30 seconds then 204; future: 409). Bootstrap/recovery uses a live
TokenReview-authorized `racer-control` bearer token; steady state uses mTLS with
local validated trust. The controller's durable `RACER_KEYRING_SECRET_NAME`
setting and issuer/keyring Secrets remain unchanged.

Upgrade the controller first and verify `/v1/keyring` on the ready leader before
rolling out the new dataplane and removing its keyring volume/mount and
`RACER_SECRET_DIRECTORY`. Keep the projected service token, controller CA trust,
and node-local identity/slab/socket mounts. Controller TLS certificate/private-key
mounts are unchanged. Rollback to an older dataplane requires restoring its
keyring projection; never reset durable credential state as part of an upgrade.

Topology lists Pods in the installation namespace using the assigned-node index
and passes those grouped lists directly to the pure `ReconcileMembers` helper.
Endpoint selection verifies node assignment and the current DaemonSet owner UID.
Membership diagnostics follow Node UID order; candidate history owns its nested
state and is installed only after the publication commits.

There is no Kubernetes abstraction, custom queue/leader-election framework,
per-node Secret, enrollment ledger, or goal-state checkpoint store. Only the
version ConfigMap (including its one-way credential initialization claim), shared
keyring Secret, and controller issuer Secret are durable controller state, alongside
the permanent installation ConfigMap, normal leader Lease, and managed workload.
See the design's Phase 3 initialization protocol and Phase 4 recovery/handoff
sections and Phase 5 serving handoff before provisioning an installation. Lost established state never
authorizes automatic reinitialization.

## Checks

```sh
make racer-generate
make racer-controller-build
make racer-server-test
make racer-manifests RACER_CLUSTER_ID=<permanent-cluster-uuid>
```

Use Go 1.26.6. `make racer-server-test` runs lint and all server/wire/deployment
race tests. `make racer-test` also checks the existing Rust contracts. Run the
opt-in integration and scale checks explicitly:

```sh
# Provision the pinned setup-envtest tool and Kubernetes assets under ./bin.
make racer-envtest-ci
# Or reuse existing assets explicitly:
make racer-envtest KUBEBUILDER_ASSETS=<absolute-repository-path>
make racer-scale
```

The targets keep test temporary files under the project. Envtest uses real etcd,
apiserver, CRD admission, TokenRequest/TokenReview, informer caches, and two managers.
Normal PR CI runs `racer-envtest-ci`, requiring successful asset provisioning and
the race-instrumented integration suite; missing assets cannot silently skip it.
Scale uses real informer indexes/deep copies against synthetic list/watch input,
real reconciliation/encoding, and fake durable CAS. It is not a Kubernetes or
HTTPS capacity test. See the design's Phase 7 results for exact measurements.

Run `make fmt` before committing. Generated deepcopy/CRD files are regenerated,
never hand-edited. Behavioral and interoperability tests accompany implementation
of each boundary; scaffold tests verify actual composition and fail-closed entry.

Standalone deployment must supply the namespace, `racer-controller-tls` serving Secret, and
`racer-bootstrap-trust` ConfigMap with `ca.crt`. The serving certificate must cover
the configured service DNS name. These public trust/server TLS inputs are separate
from the controller-managed node issuer. No sample private keys are shipped.
The server validates serving files at startup and hot-reloads valid replacements
on a one-second polling interval. In Kubernetes, that interval starts from file
visibility after kubelet Secret projection, not from the API write. Standalone
deployments use the same reloader, but an external certificate issuer and trust
owner must renew certificates and distribute compatible bootstrap trust. Hot
reload does not provide issuance or trust rotation for standalone installations.
Rotating node issuer roots are read live and remain separate from serving TLS.
Bootstrap recovery must omit expired client certificates; snapshot always requires mTLS.

## Operator installation

Installing unbounded-operator bootstraps the ClusterCache CRD. Creating a
`ClusterCache` activates Racer without requiring a Site. The operator provisions
the controller RBAC, configuration, serving TLS/trust, and initialize-only Job,
then deploys the controller after validating the consumed marker and bound
version counters. The operator deploys the dataplane DaemonSet; the controller
provisions node credentials. Both images use the operator's registry prefix and
release tag. Controller startup neither constructs nor reconciles a DaemonSet.

The operator reserves a random UUID in `racer-operator-installation`, consumes
that claim under an optimistic lock, and attempts to create `racer-installation`.
Both objects are permanent. The consumed operator claim is immutable, binds the
marker to its UID, and never authorizes marker recreation. A crash after claim
consumption but before marker creation fails closed. The controller's own
initialize command separately consumes the marker before its single counter
Create. An interrupted initialization with valid bound counters can proceed;
missing or corrupt counters require consistent recovery or an explicit new
installation with a new UUID, preferably in a new namespace. Never delete or
reset claims to retry initialization. Back up all permanent and durable objects
together; deleting every trace of an installation cannot be distinguished from
a genuinely new namespace.

Existing standalone resources are not automatically adopted. Do not mix the
standalone installation procedure with operator provisioning in one namespace.
The operator creates `racer-config` and `racer-dataplane-config` defaults only when
absent and preserves administrator data. Only installation wiring in `racer-config`
(cluster identity, endpoint/image, trust and runtime object names) is repaired with
an optimistic merge patch. Config payload hashes roll the consuming workload.
Use the existing `unbounded-component-overrides` ConfigMap with `component: racer` and
`kind: Deployment` or `kind: DaemonSet` for resources, environment, devices,
scheduling, mounts, and RDMA. Invalid overrides withhold affected workload writes;
removing valid overrides returns those fields to defaults through SSA.
Deleting the last ClusterCache retains resources and pauses workload management,
not all TLS maintenance. For an established installation with a validated permanent
operator claim, matching consumed immutable marker, and existing serving Secret,
the operator checks serving TLS hourly and maintains its bootstrap trust. This
does not resume initialization, repair workloads, or recreate deleted serving
credentials. Administrators may remove workloads deliberately; a later cache
resumes provisioning with the retained identity and counters. Standalone resources
without an operator claim are not adopted by retained TLS maintenance.

### Serving TLS rotation and recovery

The operator's serving TLS policy is fixed in code, independent of the node
issuer/shared-key policy below:

- Serving leaves expire 14 days after issuance and renew seven days before expiry,
  with a new leaf private key. Leaf validity is capped by the signing CA's expiry.
- Serving CAs expire 28 days after issuance and rotate every seven days, with a new
  CA private key and leaf. Hourly reconciliation checks the deadlines, so weekly
  key replacement depends on the operator being available.
- Previous public roots overlap for 14 days after replacement, capped by their
  expiration, with at most two previous roots. Bounded cross-signed compatibility
  chains let clients with retained old trust verify the new leaf during that
  overlap. The Secret retains only the current CA and leaf private keys, never old
  private keys. `racer-bootstrap-trust` publishes the current and retained previous
  roots, separate from the controller's node/peer issuer.

Credentials and rotation state commit together in `racer-controller-tls` under an
optimistic lock before the operator derives public bootstrap trust from the
committed Secret. Recognized legacy serving credentials migrate to weekly rotation
with an old-root compatibility bridge. Routine renewals and rotations do not
change either workload's pod template or trigger a rollout. An existing
`unbounded-cloud.io/racer-tls-hash` annotation is preserved as an inert value,
not recomputed. Config payload hashes still trigger workload rollouts.

The reloader accepts a coherent certificate/key pair, retains the last valid pair
on missing, malformed, or mixed-generation updates, and uses new credentials for
new handshakes without closing existing connections or long polls. It rejects
new handshakes when the cached leaf expires; retaining a pair does not extend its
validity. Expired optional cross-signed bridge suffixes are trimmed from validated
chains so clients trusting the current CA can still connect while the leaf remains
valid. Invalid initial files prevent listener startup. One-second polling is not
an immediate API-to-serving guarantee: kubelet projection and client trust delivery
remain asynchronous.

Continuity covers routine rotation with valid credentials and stale client trust
still inside the retained overlap, not indefinitely disconnected clients or
arbitrary operator/projection outages. An expired leaf can be renewed if the
current CA and persisted state are still valid, but any leaf-expiry outage has
already occurred. Operator downtime through the current CA's expiration requires
manual recovery of valid, consistent serving state and client trust. Missing
established serving credentials, invalid key pairs, or corrupt rotation state are
not silently replaced: restore consistent serving state rather than deleting
rotation metadata or resetting installation claims. With zero caches, a missing
serving Secret is left absent; reactivating provisioning reports it as missing.

The initial software deployment of hot reload still uses the Recreate update path
and has the control-plane interruption described below. Subsequent routine TLS
renewals require no rollout. Local rotation/reloader tests passed, but this is not
a live Kubernetes or production no-downtime validation; see the design's
[weekly serving TLS validation](../../designs/racer-control-plane.md#weekly-serving-tls-validation).

### Runtime profile and resource policy

`racer-dataplane-config` exposes the Rust environment settings, including thread,
byte-budget, and slab tuning. Defaults use HTTP (`RACER_ENABLE_RDMA=false`), eight
threads maximum (four I/O/crypto pairs), 256 MiB plaintext, 256 MiB ciphertext,
128 MiB dirty bytes, 64 MiB request contexts, and a 1 GiB hostPath slab per worker
(up to 4 GiB at four workers), split into 64 MiB segments with two free segments
reserved per slab. The 128 MiB registered budget is
unused in HTTP mode. Runtime affinity and progress checks can reduce worker pairs.
The deployed profile is parsed by a Rust test that exercises actual worker sizing
and admission. See [configuration](../racer-dataplane/CONFIGURATION.md).
Existing ConfigMap values are preserved on upgrade. Installations retaining the
older 16 MiB request-context setting can update `RACER_REQUEST_CONTEXT_BYTES` to
`67108864` in `racer-dataplane-config` to fund four workers' peer relay progress;
the config payload change rolls the dataplane.

The dataplane requests 1 CPU and 1 GiB memory. These are scheduling reservations,
not throughput guarantees or a proven RSS ceiling. No default CPU or memory limit
is imposed: TLS, metadata, stacks, allocator overhead, and filesystem cache are
outside the byte-budget sum. Set limits through generic overrides after profiling
the selected concurrency, cluster size, and RDMA configuration. The hostPath slab
is host filesystem capacity, not a Kubernetes ephemeral-storage quota. Increasing
slab capacity does not increase a pod ephemeral-storage request or limit.

The controller retains read access to Nodes, Pods, ServiceAccounts, and DaemonSets
for enrollment. Snapshot authentication uses locally validated trust. It has no
DaemonSet mutation permission. ConfigMap/Secret update and patch grants name only
`racer-installation`, `racer-version`, `racer-issuer`, and `racer-keyring`; create is
restricted to the same kind/name pairs by a fail-closed ValidatingAdmissionPolicy.
The operator installs the policy and binding before enabling the RoleBinding,
initializer, or workloads. Kubernetes must support `admissionregistration.k8s.io/v1`
ValidatingAdmissionPolicy. Racer cannot write either admin config map, serving TLS,
or the generic overrides map.

Upgrades retain the legacy DaemonSet selector, including its
`app.kubernetes.io/managed-by: racer-controller` label. That immutable compatibility
label does not confer write ownership; `unbounded-operator` is the SSA field manager.

## Standalone first installation

These templates support control-plane-only development and recovery. They no longer
install a dataplane automatically. Use operator installation for a complete Racer
deployment; do not run a second workload reconciler beside the operator.

1. Choose a **new permanent cluster UUID**, namespace, controller image, and
   compatible dataplane image. Provision the namespace and deployment TLS/trust
   described above. Keep these settings identical across initialization and normal
   operation. The serving certificate must cover
   `racer-controller.<namespace>.svc` for the default control URL.
2. Render with `make racer-manifests RACER_CLUSTER_ID=<uuid>
   RACER_NAMESPACE=<namespace> RACER_CONTROLLER_IMAGE=<image>
   RACER_DATAPLANE_IMAGE=<image> RACER_INITIALIZATION_STATE=fresh`.
   Apply `rendered/create-restriction.yaml` before RBAC or Jobs. Then apply
   `rendered/crd/`, `rendered/rbac.yaml`, `rendered/config.yaml`, and
   `rendered/installation.yaml` from `deploy/racer`. Do not start the Deployment yet.
3. Run the following initialize-only Job, substituting the namespace and controller
   image. It consumes the marker once, then creates counters 1/1. No serving TLS
   mount is needed by this command.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: racer-initialize
  namespace: <namespace>
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: racer-controller
      containers:
        - name: initialize
          image: <controller-image>
          args: [initialize]
          envFrom:
            - configMapRef:
                name: racer-config
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
```

4. Wait for `job/racer-initialize` to complete and inspect the installation marker
   (`state: consumed`, `immutable: true`) and bound `racer-version` ConfigMap.
   Rerender with the same settings and `RACER_INITIALIZATION_STATE=consumed`.
   Keep that consumed marker in deployment configuration/backups, then apply
   `deploy/racer/rendered/controller.yaml`. The leader creates credentials.
   Only the leader becomes ready; followers remain live.

Never rerun initialization to repair an established cluster. If the Job fails
after consuming the marker, inspect durable state: valid bound counters permit
normal startup; missing counters require a new cluster UUID and new installation
objects, preferably in a new namespace. Missing established issuer/keyring Secrets
also fail closed, including a partial first credential initialization. Do not
reset the marker, delete the initialization claim, or restore individual objects
from mismatched backups. The design includes the precise crash/rebootstrap rules.

## Controller updates and availability

The three-replica controller Deployment uses **Recreate**. A pod-template update
(including an image change or `kubectl rollout restart`) terminates all old
controller pods before starting replacements. Only the elected, fully initialized
leader passes `/readyz`; followers stay live through `/healthz` but unready. The
Service keeps normal ready-endpoint filtering; do not enable
`publishNotReadyAddresses` or change readiness to admit followers.

RollingUpdate is incompatible with this readiness contract when it requires any
available replica during replacement. The default three-replica budget cannot
make progress, and even `maxUnavailable: 2` leaves the last old leader blocking
replacement while new followers cannot become ready. Recreate deliberately accepts
a control-plane interruption to remove that dependency.
The rendered strategy also explicitly clears `rollingUpdate` so applying it to an
existing Deployment removes the old, API-defaulted budget.

During an update, the Service has no ready endpoint until a replacement wins the
Lease, synchronizes inputs, recovers durable state/credentials, commits a
publication, and starts its TLS listener. Lease release on shutdown is disabled,
so recovery can include waiting for the old Lease to expire, plus pod termination,
scheduling, image pulls, startup, and endpoint propagation. This is an expected
interruption under healthy cluster conditions, not a fixed outage-time guarantee;
a bad image, invalid configuration, or unavailable Kubernetes API can prolong it.

Running dataplanes retain their last accepted publication and keys while control
requests retry. Controller replacement alone does not restart the managed
DaemonSet. Membership/catalog updates, new enrollment, and certificate renewal
pause until control service returns. Existing data access still depends on valid
credentials and reachable peers; retained state does not promise indefinite
operation through a prolonged outage or a dataplane restart.

Verify an update by checking that the Deployment has observed its new generation,
all three replicas belong to the new revision, no old controller pods remain, and
one new pod is ready and present in the Service's ready EndpointSlice endpoints.
Check the leader's `/readyz` on probe port 8081 and exercise the control API through
the Service. Two live, unready followers
are expected. `kubectl rollout status` and waiting for every pod to be Ready are
not valid completion gates: Kubernetes expects all desired replicas to be
available, so those checks can time out and the Deployment can report
`ProgressDeadlineExceeded` despite a serving leader. Monitor the ready leader and
control API directly. If the replacement cannot serve, correct the configuration
or roll back to the previous working pod template; recovery uses the same Recreate
interruption. Never rerun initialization for an update or rollback.

## Cache catalog capacity and rotation

The default rotation policy (24-hour interval, 1-hour preparation, 48-hour
retention) admits **at most 356 ClusterCaches**. This is a conservative service
admission limit, not a Kubernetes object-create limit. Additional valid objects
remain stored but are omitted from the served catalog and receive no cache keys.
The controller logs `cache catalog admission rejected` with the cache name, UID,
`reason=rotation_capacity`, and computed capacity. Check these leader logs when a
created cache does not appear in snapshots; there is no per-cache status condition.

Admission reserves the 512 KiB wire budget before creating keys: both key purposes,
active and prepared generations, and `ceil(RetainFor / (Interval + PrepareFor))`
retiring generations. It includes all 20 generation-counter digits, the longest
key-state spelling, and a 1 KiB DER allowance per issuer root (enforced on generated
roots). The reserve intentionally includes a prepared generation even when the
oldest retiree would expire before preparation. Custom Go `RotationPolicy` values
therefore change the maximum. `RACER_ROTATION_INTERVAL`,
`RACER_ROTATION_PREPARE_FOR`, and `RACER_ROTATION_RETAIN_FOR` accept Go durations
with whole-second precision, retaining the defaults above. Preparation must be
positive and no longer than the interval; interval and retention are capped at
365 days. `RACER_CERTIFICATE_LIFETIME` accepts whole-second durations from two
minutes through the default 24 hours. Retention must cover that entire lifetime.
Rust renews at two-thirds of the issued lifetime (16 hours by default), capped at
16 hours. The minimum leaves a full control-poll turn before expiration.
Issuer lifetime and staged-root viability checks use the configured leaf lifetime.
For operator-managed installations, set these environment variables using a
`racer` Deployment workload override. Changing retention on an existing
installation does not shorten already persisted retirement deadlines.
Policies whose trust-root reserve alone cannot fit fail before credential
initialization consumes its one-way claim.

Existing admitted UIDs take priority, using both active key scopes in the committed
Secret as the durable admission record. Free slots are filled in ascending UID
order; a fresh installation accepts that sorted prefix. A new lower UID cannot
evict a working cache. Deleting a rejected object changes nothing; deleting an
admitted object frees a slot for the next waiting UID. Recreation has a new UID
and unrelated keys. Topology reads authoritative inputs and publishes additions
only after their keys commit; a Secret watch drives that follow-up reconciliation.
Topology, keyring reconciliation, and issuance share a context-aware, leader-local
`CatalogGate` to serialize authoritative catalog and credential operations and
prevent an in-progress topology candidate from racing key pruning. Canceled
admission preserves accepted trust and publications. Keyring and snapshot HTTPS
delivery are asynchronous independent loops, not an atomic dataplane transaction.

Capacity rejection alone preserves usable admitted credentials, rotation, and
issuer readiness. Retiring material is never evicted early to accept growth.
This is not automatic repair of previously overcommitted installations: before
upgrading an older controller or changing rotation policy, reduce the admitted
catalog to the new limit and allow old retiring generations to drain under the
old policy. An established admitted catalog above the reserve fails closed rather
than silently evicting caches. Missing/corrupt durable credentials, counter
exhaustion, and malformed catalog inputs also retain their intentional fail-closed
behavior. Do not reset initialization state to work around these failures.

## Measured limits

The 100,000-waiter test verifies bounded admission and shared publication ownership.
It does not send 100,000 HTTPS responses. Snapshot authentication checks the exact
Node certificate identity, cluster, chain, usage, and validity against locally
installed validated controller trust before and after each poll. TLS handshakes,
warm snapshots, and unchanged 204 responses make **zero Kubernetes API calls**.
There is no per-request fallback to Kubernetes. The envtest request-budget test
asserts zero requests and API-body bytes at both 1 and 1,001 live Nodes.

Live TokenReview and Pod/ServiceAccount/DaemonSet/Node authorization remain required
for enrollment, renewal, and bearer keyring bootstrap/recovery. An issued certificate remains usable after workload
deletion, Node recreation, or exclusion until expiration or trust retirement.
Membership controls routing, not authorization. Startup re-enrollment and renewal
still resolve recreated Node identities; a changed UID fences and restarts the
Rust runtime rather than rebinding an active graph.

Enrollment has its own bounded admission pool (32 by default), separate from the
equally bounded local TLS/snapshot authentication pool. Slow Kubernetes API calls,
credential-gate waits, and incomplete enrollment bodies cannot consume local
authentication capacity. Enrollment saturation returns HTTP 429 after TLS setup;
local authentication saturation can still reject a handshake. This isolation does
not increase concurrent enrollment API work or remove its request deadline.

Trust is installed by controller reconciliation after validating committed durable
credentials and installation binding. Observed invalidity or deletion withdraws
trust; subsequent API read failures cannot restore it. Temporary API read outages
may serve previously accepted local state while leadership holds. Leadership loss,
certificate expiration on pooled connections, and trust rotation remain enforced.
See `designs/racer-control-plane.md` for the measurement setup and remaining costs.

Production `Run` uses controller-runtime v0.25.1's `ctrl.GetConfig()`, which sets
default QPS to -1 (client-side throttling disabled), rather than client-go's
standalone 5-QPS/10-burst defaults. Envtest uses QPS=1000/burst=2000; its timings
do not establish production capacity. Authentication is bounded to 32 concurrent
operations and snapshot writes to 128; overload returns 429. API priority/fairness,
credential size, informer memory, full snapshot bandwidth, and cache convergence
remain operational limits. Envtest cannot verify kubelet trust/token projection,
host-directory permissions, scheduling, or client runtime behavior.
