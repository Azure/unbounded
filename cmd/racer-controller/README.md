# Racer controller implementation status

Phases 1-7 implement and verify the server: configuration, bounded codecs, membership/catalog calculation,
one-shot initialization, durable publication CAS, issuer/shared-key rotation, and
certificate issuance, TokenReview bootstrap, and operational leader-scoped HTTPS/mTLS
serving, and managed DaemonSet reconciliation. Phase 7 exercises real Kubernetes
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
- `internal/racer`: three ordinary controller-runtime reconcilers, manager,
  publication state, token bootstrap, certificate issuance, and mTLS server.
- `internal/racer/wire`: shared contract declarations and bounded codec boundaries.
- `deploy/racer`: controller/RBAC/config templates. Workload reconciliation owns
  the dataplane DaemonSet; supply a compatible image.

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
# Binaries may be reused from repository-local tooling. If absent, install
# setup-envtest into ./bin and download assets into ./bin/envtest, not $HOME.
mkdir -p bin tmp
export TMPDIR="$PWD/tmp"
GOBIN="$PWD/bin" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37.0 --bin-dir "$PWD/bin/envtest" -p path)"
make racer-envtest
make racer-scale
```

The targets keep test temporary files under the project. Envtest uses real etcd,
apiserver, CRD admission, TokenRequest/TokenReview, informer caches, and two managers.
Scale uses real informer indexes/deep copies against synthetic list/watch input,
real reconciliation/encoding, and fake durable CAS. It is not a Kubernetes or
HTTPS capacity test. See the design's Phase 7 results for exact measurements.

Run `make fmt` before committing. Generated deepcopy/CRD files are regenerated,
never hand-edited. Behavioral and interoperability tests accompany implementation
of each boundary; scaffold tests verify actual composition and fail-closed entry.

Deployment must supply the namespace, `racer-controller-tls` serving Secret, and
`racer-bootstrap-trust` ConfigMap with `ca.crt`. The serving certificate must cover
the configured service DNS name. These public trust/server TLS inputs are separate
from the controller-managed node issuer. No sample private keys are shipped.
The serving files are loaded at startup, so replacing deployment TLS certificates
requires a controller restart. Rotating node issuer roots are read live. Bootstrap
recovery must omit expired client certificates; snapshot always requires mTLS.

## Operator-managed installation

When `unbounded-operator` is installed, create a cluster-scoped `ClusterCache`
to install Racer automatically. No Site is required:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: default
spec: {}
```

The operator bootstraps the ClusterCache CRD and provisions Racer in its own
namespace. It persists a permanent cluster UUID, creates serving TLS and bootstrap
trust, applies RBAC and configuration, and runs `job/racer-initialize` once with
retries disabled. After validating the consumed installation marker and its bound
version counters, it installs `deployment/racer-controller`. The elected Racer
controller creates and maintains `daemonset/racer-dataplane`. Both images use the
operator's configured registry and version tag.

While any ClusterCache exists, the operator repairs the controller installation
and renews serving certificates. Configuration and certificate changes restart
the controller using a Recreate deployment strategy: only the leader is ready, so
a rolling update would stall waiting for a ready replacement follower. Expect a
control-plane interruption during these updates. The `racer` Deployment supports
the operator's workload overrides.

Deleting the last ClusterCache retains all Racer resources and stops operator
reconciliation of the installation. To remove the running components manually,
first ensure there are no ClusterCaches, then stop the controller and wait for
its pods before deleting the dataplane. Otherwise the controller will recreate
the DaemonSet:

```sh
kubectl get clustercaches
kubectl -n unbounded-system delete deployment racer-controller --cascade=foreground --wait=true
kubectl -n unbounded-system wait --for=delete pod -l app=racer-controller --timeout=120s
kubectl -n unbounded-system delete daemonset racer-dataplane --wait=true
```

Use your operator namespace if it differs from `unbounded-system`. Retain the
ConfigMaps `racer-installation`, `racer-version`, `racer-config`, and
`racer-bootstrap-trust`, and Secrets `racer-controller-tls`, `racer-issuer`, and
`racer-keyring`. A later ClusterCache recreates the workloads using this identity
and counter history, without rerunning initialization. Missing or corrupt durable
state fails closed; restore consistent state rather than deleting the marker or
rerunning the initialization Job. Inspect operator logs and the initialization
Job if installation does not complete. On Sites, `RacerReady` reports the
operator's reconciliation result, not workload availability.

## First installation without the operator

1. Choose a **new permanent cluster UUID**, namespace, controller image, and
   compatible dataplane image. Provision the namespace and deployment TLS/trust
   described above. Keep these settings identical across initialization and normal
   operation. The serving certificate must cover
   `racer-controller.<namespace>.svc` for the default control URL.
2. Render with `make racer-manifests RACER_CLUSTER_ID=<uuid>
   RACER_NAMESPACE=<namespace> RACER_CONTROLLER_IMAGE=<image>
   RACER_DATAPLANE_IMAGE=<image> RACER_INITIALIZATION_STATE=fresh`.
   Apply `rendered/crd/`, `rendered/rbac.yaml`, `rendered/config.yaml`, and
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
   `deploy/racer/rendered/controller.yaml`. The leader creates credentials and the
   managed DaemonSet. Only the leader becomes ready; followers remain live.

Never rerun initialization to repair an established cluster. If the Job fails
after consuming the marker, inspect durable state: valid bound counters permit
normal startup; missing counters require a new cluster UUID and new installation
objects, preferably in a new namespace. Missing established issuer/keyring Secrets
also fail closed, including a partial first credential initialization. Do not
reset the marker, delete the initialization claim, or restore individual objects
from mismatched backups. The design includes the precise crash/rebootstrap rules.

## Measured limits

The 100,000-waiter test verifies bounded admission and shared publication ownership.
It does not send 100,000 HTTPS responses. Real snapshot authorization currently
performs two full Node lists and 14 other API reads per successful response on a
warm TLS connection. Authentication is bounded to 32 concurrent operations and
snapshot writes to 128; overload returns 429. A 100,000-node HTTPS capacity claim
has not been established. Envtest cannot verify kubelet Secret/token projection,
host-directory permissions, scheduling, or client runtime behavior.
