# Racer-only Zipf benchmark

## Final configuration

100 loadgens use direct UDS on `agentpool=ddsv6`, sharing a deterministic
16 TiB catalog: 16384 blobs of exactly 1073741824 bytes (1 GiB) each.
The final loadgen arguments in `daemonset-patch.yaml` are:

```text
--backend=uds --volume=racer-loadgen --metrics-listen=:9090
--catalog-blobs=16384 --blob-bytes=1073741824 --catalog-workers=32
--seed=zipf-balanced-v2 --profile=zipf --zipf-exponent=1.1
--concurrency=16 --concurrency-file=/etc/racer-loadgen-control/concurrency
--blob-concurrency=1 --pull-timeout=2m --retry-delay=1s --interval=0s
--verify=false --start-delay=10s --startup-timeout=120m
```

The Pod-template benchmark label is `racer.unbounded-cloud.io/benchmark=zipf-16t-v3`.
`control.yaml` and the overlay startup fallback both select C16. Change live
concurrency through `racer-loadgen-control`, not the Pod template: restarting a
loadgen hashes the catalog again. Leave the current loadgens and C16 running.

Image tags:

- Loadgen: `ghcr.io/azure/racer-loadgen:ee7aa7d2ab336da93106cc0d7f10874f71fb8207`.
- Controller and dataplane: `ghcr.io/azure/racer-{controller,dataplane}:f9a088a22a29cf8549e1945af833f508a14199d1`.

The reported loadgen image build, GitHub Actions run `38062921220`, succeeded.
The loadgen tag matches `kustomization.yaml` and `render_test.py`; SHA tags are
not digest pins. Confirm resolved image digests before any separately authorized
deployment. Do not change the current image pins.

### Startup bounds and source contract

Every loadgen hashes all 16 TiB before origin readiness. Health and metrics start
first; readiness stays false during hashing. The application startup deadline is
120 minutes. The readiness startup probe allows 780 attempts at 10-second
intervals (130 minutes). This is not a load duration; no `--duration` is set.
Observe startup in bounded five-minute phases, not one full-length rollout wait.

`--catalog-workers` accepts 1-64 workers and requires `--catalog-blobs`.
The catalog limit is 32768 blobs. The configured 32 workers bound hashing
parallelism; each reuses a 128 KiB scratch buffer, and only metadata survives
hashing. No CPU or memory limits are added for these workers.

Source references, relative to the repository root:

- Flags and validation: `cmd/racer-loadgen/main.go:66-70,140-145`.
- Catalog limits, worker buffers, and one-blob operations:
  `cmd/racer-loadgen/blob.go:22-26,95-164`.
- Health serving, startup deadline, catalog generation, and readiness:
  `cmd/racer-loadgen/main.go:266-296,335-374`.
- Unit assertions for worker bounds, stable content, and catalog size:
  `cmd/racer-loadgen/blob_catalog_workers_test.go:20-88`.

### Dataplane budgets

`racer-overrides.yaml` sets 48 GiB plaintext, 64 GiB ciphertext, 2 GiB each
for dirty and registered buffers, and 1 GiB for request contexts. Count quotas
remain flights 2048, queue entries 16384, client connections 16384, pipes 1024,
and relay transfers 1024. Per-neighbor connections are 8, peer inflight maximum
1024, peer per-neighbor maximum 128, and active path searches 64.
Optional path-cache settings and automatic worker sizing remain unchanged.
The target nodes use 21 I/O and 11 crypto workers, with no Kubernetes CPU or
memory limits. Check final Pod specs and namespace LimitRanges for injected limits.

Plaintext and ciphertext budgets provide about 2.29 and 3.05 GiB per I/O worker.
Client connections are divided by I/O workers, then outbound connections get one
quarter: 195 slots per worker. See `cmd/racer-dataplane/src/app.rs:2399-2418`
and `cmd/racer-dataplane/src/admission.rs:134-142`.

`RACER_ADMISSION_MODE=second-sight` remains the disk-retention policy, not a new
memory-admission policy. No second-sight memory admission was added. Repeated
nonowner reads can retain verified ciphertext; resource admission and integrity
checks still apply. The mode default is in `cmd/racer-dataplane/src/config.rs:177-181`
and its retention wiring is in `cmd/racer-dataplane/src/app.rs:1281-1289`.
Disk data, devices, count quotas, and other buffers remain unchanged.

## Latest reported results

Two fresh windows with the final configuration reported:

| Window | Memory events | Disk events | Peer events | Successful payload |
| --- | --- | --- | --- | --- |
| 1 | 29.47% | 42.40% | 28.13% | 3.965 Tbit/s |
| 2 | 30.18% | 42.50% | 27.32% | 4.069 Tbit/s |

The final drift check counted 60,957 successes and zero errors across all 100 nodes.
These are warm-up observations, not steady-state balance: indexed disk payload
was still growing by about 4.5 TB over 130 seconds. Earlier windows had 31-32%
memory, 34-37% disk, and 32-34% peer events; disk share has since increased.
All categories remain active, but the ideal 25-40% range no longer holds for disk.
They do not establish a
sustained balance improvement from the plaintext budget change.

Use **1073741824 bytes per completed successful operation** for logical
throughput (`cmd/racer-loadgen/blob.go:149`, `cmd/racer-loadgen/pull.go:251-281,314-324`).
This is not wire traffic or client-verified goodput. Memory, disk, and peer shares
are nonexclusive hit events, not client-read outcomes; a peer page can also count
on its serving node. Exclude rollout and startup from measurement.

Loadgen Pods carry label `racer_unbounded_cloud_io_benchmark="zipf-16t-v3"`.
Do not label old Pods or reuse this label for another size or exponent. Missing
labeled successful-counter samples mean no data, not zero. Check coverage of all
100 loadgens and wait for a full five-minute window after readiness.

## Rollout and NVMe safety

This configuration is a reproduction reference, not authorization for another
rollout, load pause, disk reset, or loadgen restart. Both benchmark DaemonSets
use 100% maximum unavailability and zero termination grace. These settings can
interrupt all reads and lose recent checkpoints; zero grace is not a safe drain.

**Verify all 300 approved NVMe devices across all 100 nodes after every dataplane
rollout.** Prior rollouts left devices busy and caused file-slab fallback.
Readiness alone does not prove raw-device use. Resolve IDs from current node
annotations, check selected identities and raw-device logs, and confirm all
100 dataplanes use the expected workers and resource settings.

If fallback occurs, recover only affected Pod UIDs after old processes exit and
read-only exclusive-open checks pass for all three approved devices on each
affected node. Preserve loadgen specs, UIDs, restart counts, and C16. Never wipe
disks, edit guards, change selectors, or retry the whole fleet blindly. Keep each
recovery phase bounded and record the exact pending checkpoint.

No generic disk selector is supplied. For a new installation, approve explicit
per-node device IDs, excluding OS disks, partitions, mounts, holders, and protected
data. Never clear device guards automatically. See the
[raw-device reference](../../../docs/content/reference/racer.md).

## Reproduction checks

Run from the repository root; these commands are local checks, not deployment:

```sh
timeout --signal=TERM --kill-after=10s 280s python3 -B deploy/racer-loadgen/zipf/render_test.py
timeout --signal=TERM --kill-after=10s 60s kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/racer-loadgen/zipf
```

Do not reapply the overlay to control the active experiment: it selects C16 and
may restart loadgens. `zipf-paused` is for a separately authorized new deployment.
The loadgen overlay alone does not install Racer. On the previously checked
Racer-free operator v0.8.0 installation, use the separate bootstrap path in
`hack/cmd/racer-bootstrap`, not shared operator overrides. A compatible manager
must own Racer workloads before using the local overrides; preserve unrelated
ConfigMap entries. Never reset installation identity or run competing managers.
