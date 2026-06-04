# gantry — end-to-end test suite

This directory holds the kind-based integration suite. It boots a real
Kubernetes cluster on Docker, builds the gantry container image, and
deploys the DaemonSet. The current smoke scenario asserts rollout
readiness, installs containerd mirror config, pulls through Gantry on
one worker, then verifies a second worker reuses warmed content through
peer fetch.

## Status

- ✅ Smoke test — DaemonSet rolls out, all pods reach `/readyz=200`.
- ✅ Pull-through + warm peer reuse — installs `hosts.toml`, pulls
   `registry.k8s.io/e2e-test-images/agnhost:2.39` on two workers, and
   asserts advertise + peer-fetch metrics increase.
- ✅ Cold-start designated origin puller — two concurrent pulls
   across two workers, asserts the **per-digest HRW invariant**: every
   blob's `please_pull served` log line appears on at most one pod (no
   digest is origin-pulled twice cluster-wide). HRW is per-digest, so
   work naturally distributes across nodes — different blobs of the
   same image can land on different pullers; what's prevented is
   N nodes pulling the *same* blob.
- ✅ Eviction / stale-provider recovery — pull then evict on node A,
   trigger pull on node B, assert peer fetch sees `outcome="notfound"`,
   `gantry_stale_provider_filtered_total` increases, origin fallback
   succeeds, and pod becomes ready.
- ✅ Private / authenticated registry — htpasswd-protected `registry:2`
   fixture proves credential flow end-to-end through the mirror.
- ✅ Lease lifecycle — gantry-managed containerd leases visible after
   background pull.
- ✅ NetworkPolicy hardening — kind-friendly NP applied, pod still
   reaches `/readyz=200`.
- ✅ Containerd socket access — log + `/readyz` probe confirms the
   socket is reachable on the kind node image. Real production node
   images must still validate this on their target node pool.

Additional scenarios listed below should each land as their own commit.

## Prereqs

The harness shells out to standard CLIs; no extra Go deps. Install:

- [Docker](https://docs.docker.com/get-docker/) (engine running)
- [kind](https://kind.sigs.k8s.io/) ≥ 0.20
- [kubectl](https://kubernetes.io/docs/tasks/tools/) ≥ 1.28
- Go ≥ 1.26 (matching root `go.mod`)

`make tools-e2e` checks every binary is on `$PATH` and fails loud if
anything is missing.

## Running

```sh
make tools-e2e        # one-time prereq check
make e2e              # boot kind, build+load image, deploy, run tests, tear down
```

To keep the cluster running after the test (for debugging), set
`E2E_KEEP=1`:

```sh
E2E_KEEP=1 make e2e
# ...
kind delete cluster --name gantry-e2e
```

Test logs land in `e2e/.artifacts/<test-name>.log`. The harness also
dumps `kubectl describe pods` + container logs on failure.

## How it works

The harness (`harness_e2e.go`) is a small Go-driven wrapper over the
prereq CLIs:

| Step | What it does |
| --- | --- |
| `bootCluster()` | `kind create cluster --config kind-config.yaml` |
| `buildAndLoadImage()` | `deploy/build.sh -t e2e` then `kind load docker-image gantry:e2e` |
| `applyManifests()` | rewrites the DaemonSet image to `gantry:e2e` then `kubectl apply -f deploy/` (NetworkPolicy is intentionally NOT applied — `deploy/examples/networkpolicy.yaml` is a templated production reference with placeholder CIDRs that fail validation in kind; a kind-friendly hardening overlay is a separate work item) |
| `waitForRollout()` | polls `kubectl rollout status ds/gantry -n gantry-system` |
| `checkReadyz()` | port-forwards one Gantry pod and curls `/readyz` on port 9095 |
| pull-through check | installs `hosts.toml` on each kind node, removes the test image from node-local containerd, schedules a pull on worker A, waits for advertise metrics, then schedules the same image on worker B and waits for `p2p_peer_fetch_total{outcome="hit"}` |
| `teardown()` | `kind delete cluster` (skipped when `E2E_KEEP=1`) |

The smoke test also installs a `hosts.toml` mirror entry for
`registry.k8s.io`, removes the test image from kind nodes, pulls
`registry.k8s.io/e2e-test-images/agnhost:2.39` on one worker, waits
for Gantry advertisement, then pulls the same image on a second worker
and asserts peer-fetch metrics increase.

The kind config (`kind-config.yaml`) declares one control-plane + two
worker nodes — enough to exercise multi-peer coord paths in future
scenarios.

## Build tag

All e2e files carry `//go:build e2e`. Default `go test ./...` skips
them. Run with:

```sh
go test -tags=e2e ./e2e/... -v -timeout=10m
```

The `make e2e` target sets `-tags=e2e` and a generous timeout for you.

## Future scenarios

The scenarios below are still gaps. Each should land as a focused commit.

1. **Origin failure → cluster-wide circuit (§5.8).** Inject 401/429 via a
   mock upstream and assert peers honor the propagated cooldown.
2. **NF5 pod-kill simulation.** `kubectl delete pod` mid-pull and assert
   the designated-puller takeover metric increments
   (`p2p_designated_puller_takeover_total`).
3. **DHT-empty cold-start hold (§7.7).** Start with one node, pull, and
   assert no ungated origin fallback before `bootstrap_window` elapses.

## Non-goals

- **Canary / mixed rollout / rollback.** Running a subset of nodes on
  the new image while the rest run the previous release is
  unsupported and there is no rollback path. `storage_mode=containerd`
  is the only accepted storage mode (plan §Phase 8 removed the
  alternative `gantry-cache` hostPath backend); a mixed-version
  rollout that mixes incompatible coord / advertise / cdsub semantics
  is left to the operator's pre-deploy validation rather than an
  in-cluster compatibility envelope.

## Caveats

- The kind cluster boot takes ~60–120 s. The Makefile target reserves
  a 10-minute test timeout to absorb that.
- The default kind containerd uses namespace `k8s.io`, matching the
  gantry `containerd_namespace` default — no extra config needed.
- Containerd socket access is mandatory. The default DaemonSet runs with
   UID 65532 and primary GID 0 to work with common `root:root 0660`
   containerd sockets, and the kind smoke covers that path. Clusters
   that use a dedicated socket group should patch `runAsGroup`/`fsGroup`;
   clusters that forbid GID 0 need a site-specific ownership or privilege
   strategy. Because readiness pings the containerd content store,
   misconfigured permissions surface as a permanent 503 — `TestE2E_
   ContainerdSocketAccess` proves the kind node image, but the target
   production node pool must be validated separately before broad prod.
