# Racer operator integration e2e

Imported from `racer/racer-controlplane/e2e` at
`c9bf09848a66df58d7cde6bb09bd5c6fcc61913c`. Go fixtures belong to the root
`github.com/Azure/unbounded` module. All images build with the repository root
as their context. The Python origin and vLLM loader retain the upstream
checkpoint, real S3 requests, and canonical SHA-256-to-S3-ETag translation.

## Coverage

`TestDeployment` builds and installs the real `unbounded-operator` using its
shipping templates and RBAC. Operator startup installs the current embedded
CRDs. A `unbounded-cloud.io/v1alpha3` Site with `components.racer.enabled: true`
causes the operator to create the shared control plane and per-Site DaemonSet.
No standalone Racer installation manifests are applied.

The suite preserves the upstream checks for:

- Successful bootstrap, signed `/v2` configuration and coordinated activation;
  removed legacy endpoint rejection and unauthenticated `/v2` rejection.
- Service listener allocation, Local routing, Ready EndpointSlices, numeric
  origin updates and functioning dataplanes with deliberately unusable DNS.
- HEAD, GET, ranges, missing objects, exact raw request targets, cache hits,
  single-owner origin fetches, and authenticated peer forwarding.
- Two staged signing-key rotations with continuous traffic and leader failure;
  signing Secret persistence through leader replacement and controller restart.
- Origin Service IP replacement without invalidating warm cache, cache-generation
  changes, independent volume addition/removal, and dataplane replacement with
  stable bootstrap identity.

Additional live checks exclude and re-enroll a Node, move it between two enabled
Sites, assert isolated active volumes and EndpointSlices, disable the second
Site, and return the Node to the first Site. Membership uses
`unbounded-cloud.io/site` and `racer.unbounded-cloud.io/exclude=true`; no Node
universe or deployment-profile opt-in is installed.

`TestVLLMS3` uses independent CPU vLLM clients on the two worker networks. It
loads actual safetensors into parameters, verifies exact tensors and inference,
asserts the cold HEAD/two-range-GET ledger, then proves the second load is warm
and peer forwarding occurred. This uses Moto as the S3 backend, not a GPU model
server.

`TestOperatorFixturePlan` runs the real Racer constructors through override
parsing, validation, and merging without Kubernetes. It checks that the fixture
keeps Unconfined, daemon startup, and shipping Guaranteed CPU/memory resources, and
that the real net constructors accept the parking overrides.
`TestOperatorInstallation` renders the shipping templates and checks the installed
operator image, namespace, ServiceAccount/RBAC binding, and configuration. These
are offline fixture checks, not evidence of a passing live deployment.

`TestFixtureVolumeUniverses` checks every example Service and the programmatic
volume builder for a nonempty `racer.unbounded-cloud.io/universe` annotation
matching the selector. The annotation is required independently of the selector.
Live fixture application performs the same check before sending typed Services
to Kubernetes. The fixture Make target first renders `net-manifests`, ensuring
the real net workloads are embedded even on a fresh checkout.

## Prerequisites

- Linux, Docker daemon access, `kind`, `kubectl`, and Go 1.26.6.
- A host capable of running the Racer dataplane: Linux 6.1+ with io_uring,
  cgroup v2, 4 KiB pages, ext4 scratch storage, NUMA memory binding, at least two
  distinct physical cores in the participating NUMA node, and permission to
  raise memlock to 256 MiB. Each worker dataplane and its bootstrap retain
  requests/limits of 3 CPUs and 2 GiB. Allow capacity for Kubernetes and fixtures.
- Enough disk for root-context image builds and kind images. The vLLM CPU image
  is large. Allow space for each worker's 128 MiB test slab.
- Enough host inotify instances for three additional kind nodes. Exhaustion can
  prevent systemd from booting before an API server exists.
- Registry access for the base images and Python dependencies, or cached layers.

The main dataplane remains **Unconfined**, drops capabilities except
`SYS_RESOURCE`, and sets memlock before starting `/usr/local/bin/racer-dataplane`.
Bootstrap uses the existing
`/usr/local/bin/racer-controlplane` alias. The root binaries remain the images'
shipping `/racer-*` binaries. No custom seccomp profile is installed or mounted.

Workload overrides select local images, reduce control-plane CPU/memory requests,
shorten signing propagation to two minutes, disable dataplane DNS, and select
a smaller slab on an additional ext4 hostPath. The operator's original volume
is preserved because overrides protect declared volume identities. Optional
Site components are explicitly disabled. Since net is an unconditional cluster
singleton, its Deployment is scaled to zero and its DaemonSet gets an unmatched
node selector through overrides, preserving kind's CNI.
The override ConfigMap is seeded before the operator starts so its initial cache
sync observes the parking policy before any Site is created.

## Commands

Run from the repository root:

```sh
export GOTOOLCHAIN=go1.26.6

# Compile all e2e packages without running live tests.
make e2e-racer-compile

# Offline fixture checks, including the real operator override pipeline.
make e2e-racer-fixtures

# Scoped formatting and lint only.
golangci-lint fmt ./e2e/racer/...
golangci-lint run ./e2e/racer/...

# Build images, create a fresh three-node kind cluster, and run live tests.
make e2e-racer
make e2e-racer-vllm

# Independent origin-contract test; does not run the vLLM end-to-end suite.
docker build -t racer-vllm-origin:test -f images/racer-vllm-origin/Containerfile .
docker run --rm --entrypoint python3 \
  -v "$PWD/e2e/racer/vllm:/tests:ro" -e PYTHONDONTWRITEBYTECODE=1 \
  racer-vllm-origin:test -m unittest discover -s /tests -p test_origin.py -v
```

`RACER_E2E_DIR` selects an existing/ext4 scratch location; the default is
`e2e/racer/.artifacts`. `RACER_E2E_NODE_IMAGE` defaults to
`kindest/node:v1.33.1`. `RACER_E2E_KEEP=1` preserves the suite's cluster and images.
The test logs the kubeconfig and cluster name. Otherwise cleanup deletes only
the suite's uniquely named cluster and images; failed runs retain diagnostic
files and cache directories. Diagnostics include operator/container logs, Site
and Node state, resources, events, dataplane status/metrics, and kind boot logs.

## CI

`.github/workflows/ci.yaml` compiles the entire e2e suite and runs offline fixture
checks in the Racer job. That job installs envtest using `setup-envtest@release-0.25`,
selects Kubernetes **1.37.0**, exports `KUBEBUILDER_ASSETS`, and explicitly runs
`TestB14RealAPICAS`, `TestControllerAPIIntegration`, `TestSiteWorkloadAdmission`,
and `TestAPIDefaultedResourcesAreNoOp` with the race detector. Asset download or
API startup failures fail the job rather than silently skip those tests.
A separate `Racer E2E (deployment)` job on a fresh
`ubuntu-24.04` runner runs `make e2e-racer` for every CI event. It checks ext4,
cgroup v2, 4 KiB pages, at least two physical cores on the first NUMA node,
four allowed CPUs, and 12 GiB RAM before building. The real daemon exercises
worker placement, storage, buffer pools, and io_uring during startup.
Missing prerequisites fail the job explicitly rather than skip live assertions.
The job records inotify settings without changing host sysctls.

The CI workflow's manual `racer_vllm` boolean adds a separate vLLM matrix job
with its own runner. It defaults to false and is absent from ordinary PR/push
runs. Both jobs upload logs and failure diagnostics with `if: always()`, including
hidden artifact directories, excluding sparse slab cache directories and the
test kubeconfig. Console output is preserved even when image building fails
before cluster creation. CI execution itself must still be verified after push.

## Workload examples

- `examples/site.yaml`: Site enrollment for a kind-like external-CNI cluster;
  adjust CIDRs to the target environment. Install the operator first.
- `examples/volume.yaml`: 64-slot volume backed by a Service named `origin`;
  used by the suite after removing the pinned listener to test allocation.
- `examples/loadgen.yaml`: imported load generator/origin DaemonSet and Services,
  adapted to Site labels, exclusion, current metadata, and `unbounded-system`.
  Build `images/racer-loadgen/Containerfile` from the root, make the image
  available on nodes, and tune workload size before applying.

## Validation status (2026-09-21)

- E2E-tagged compilation, fixture contract, operator fixture-plan check, and
  scoped Go lint passed using `GOTOOLCHAIN=go1.26.6`.
- `make e2e-racer-compile e2e-racer-fixtures` passed, including the rendered
  operator installation, net parking, and explicit volume universe checks.
  The universe regression reproduced both missing-annotation defects before
  their fixes. The live targets' commands were
  verified with `make -n e2e-racer e2e-racer-vllm`.
- Actionlint v1.7.12 passed with the project's `-shellcheck= -pyflakes=` policy.
- Root-context builds succeeded for control plane, dataplane, fixture, operator,
  vLLM origin, and vLLM client.
- The Moto-backed Python origin contract test passed for canonical checksum
  translation, exact two-page range data, and stale/wrong validator rejection.
- All four controller/operator API tests listed above passed locally with
  Kubernetes 1.37.0 envtest assets and `-race`, including API-defaulted no-op SSA
  and live drift repair. Envtest does not exercise kind or the dataplane.
- `TestDeployment` was attempted. It stopped during kind node preparation with
  `could not find a log line that matches "Reached target .*Multi-User System.*|detected cgroup v1"`.
  All three node serial logs report
  `Failed to create control group inotify object: Too many open files` and
  `Failed to allocate manager object: Too many open files`.
  The host reported `fs.inotify.max_user_instances = 128`.
  Artifacts: `.artifacts/racer-kind-760405845/kind-logs/`.
- Live deployment/protocol/membership assertions were **not reached**.
  `TestVLLMS3` was **not run** against Kubernetes because the same kind boot
  prerequisite was unavailable. Image builds are not a substitute for either.
- A bounded single-node kind probe also failed before boot with the identical
  inotify error; reducing the live topology would not resolve it. Its serial log
  is `.artifacts/inotify-single-node/racer-inotify-probe-1790031000-control-plane/serial.log`.
  The probe cluster was deleted. `unshare --user --map-root-user true` failed with
  `write failed /proc/self/uid_map: Operation not permitted`, so an unprivileged
  rootless workaround is unavailable in this environment. No host sysctls or
  other running clusters were changed; all two-worker/multi-Site assertions remain.
