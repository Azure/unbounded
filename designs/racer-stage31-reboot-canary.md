# Stage31: single-node reboot did not repair MANA TX bottleneck

September 29, 2026, 11:56-12:08 UTC. Operational canary 00005f, unchanged
control 00000i. Exactly one authorized normal guest reboot; no fleet extension.

## Decision

**Reboot recovery succeeded, transport repair failed. Underlay retry is not
cleared. Do not repeat the reboot or extend this failed repair to the other ten
affected nodes.** No application fix, build, test suite, image change, or rollout
was performed. This result does not establish a firmware defect.

The live read-only inspection exposed no devlink device or health reporter and
no ethtool coalescing support. PCI sysfs advertised FLR, but availability alone
does not establish a safe live driver-coordinated EQ/MSI reset. No FLR, driver
unbind, VF disable, PCI remove, or speculative reset was attempted. The running
driver is built-in MANA, kernel 6.8.0-1067-azure, package 6.8.0-1067.75.
Evidence: `tmp/racer-stage31-reset-inspection.json:2-16`.

## Actual before and after

Same origin process role on 00005f, receiver in 00000i host network namespace,
same digest `sha256:2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`,
same range bytes=0-16777215. Each transfer returned HTTP206 and 16,777,216 bytes.
Origin PodIP naturally changed from 10.244.12.160 to 10.244.12.171; fresh client
ports avoided TIME_WAIT. NIC rates include whole-node C6 traffic, not just the
diagnostic transfer. The 120-second observation was passive, after catalog512
initialization and C6 readiness, without tracing.

| Observation | Transfer seconds | Canary TX Gb/s | Stops / observed seconds | Qdisc drops | Ending backlog bytes | Control TX Gb/s / drops |
|---|---:|---:|---:|---:|---:|---:|
| Before reboot | 14.080 | 1.947 | 523568 / 15.682 | 12155 | 2413936 | 6.638 / 0 |
| After catalog ready | 13.350 | 1.912 | 492139 / 14.903 | 10744 | 2448526 | 6.984 / 0 |
| C6 sustain | n/a | 1.965 | 4046240 / 120.023 | 83868 | 2548117 | 6.757 / 0 |
| Later transfer | 9.442 | 2.022 | 384345 / 11.124 | 5639 | 2685130 | 6.808 / 0 |

All eight TX queues progressed. Exposed TX CQ error and unknown-type counters
did not increase during the three transfers. Sender final TCP retransmission
counts were 75 before, 74 after, and 45 later; PMTU1442/MSS1390 and nonzero send
windows persisted. Thus the later faster sample is not sustained recovery.
Evidence: `tmp/racer-stage31-analysis.json` and raw
`racer-stage31-{baseline,after-transfer,sustain,sustained-transfer}.json`.

Read-only 997Hz sampling reused stage30's running-kernel-BTF-verified layouts.
Pending-send samples without the next CQE ready were 34320/39465 (86.96%) before
and 34068/38906 (87.56%) after reboot. Control was mostly empty (34412 and34600
samples); conditional pending/not-ready fractions were22.30% and25.00%.
This is non-atomic sampled readiness, not submission-to-DMA latency or proof
of device-side causation. Raw traces and histograms remain in
`tmp/racer-stage31-{before,after}-readiness.json`; analysis is in
`tmp/racer-stage31-analysis.json`. Both tracers exited and detached.

## Preservation and bounded recovery

- NodeUID retained: `cde84a72-477b-4aa9-abe7-6f0b9564473d`.
- Boot changed from `1aabc992-86e7-4683-880b-62b17191e225` to
  `18bc5879-f89f-47ca-9b21-7738cb0670e3`.
- Journal shows normal shutdown/unmounts and reboot.target at11:57:31, new boot
  at11:57:38, MANA registration and VF datapath restored at11:57:40
  (`tmp/racer-stage31-audit.json:3-5`).
- Independent observer was started outside the canary before issuing the
  restricted pod. The pod had exact nodeName, hostname/machineID/old-boot guards,
  hostPID for systemd bus authentication, no hostNetwork or service-account
  token, read-only root/host mount, SYS_CHROOT as its only capability,
  restartPolicy Never, and180-second active deadline. No force reboot or drain.
- Catalog rehash progressed normally through all512 images; origin ready and
  applied concurrency6 logged at12:04:06. All three local apps Ready by12:04:26.
  Bounded polls returned to the agent between intervals; no repeat reboot.
- Full1500-node comparison found only00005f's boot changed, no NodeUID changes,
  unchanged node set, canary/control providerIDs and machineIDs. Observed disk
  serials and filesystem UUIDs compare equal. These guest identifiers are not
  Azure managed-disk resource IDs; no cloud disk operation was issued.
- RX1024/TX256, eight channels, offloads, budgets, irqbalance active, and kernel
  unchanged. RSS indirection/function unchanged; the canary RSS hash key changed
  across boot and was not manually restored. IRQ state was recreated and its
  effective affinities captured. Do not claim every volatile setting identical.
- Target app pod UIDs/specs unchanged. Canary Gantry/loadgen restart counts0->1,
  DP6->7; control counts unchanged. Runtime secure-directory init succeeded:
  parent root:root0755, client0750, origin0755. No manual directory intervention.
- Gantry identity-key metadata unchanged. Racer identity file size/mtime changed
  on startup; no credential-byte-preservation claim. No secret values exported;
  namespace Secret names/UIDs/resourceVersions/types compare equal.
- 00007r node spec, labels, annotations, and boot compare equal. Quarantine was
  untouched. No delete/reimage/redeploy, application configuration, concurrency,
  security-policy, conntrack, or persistent NIC setting change.

Evidence: `tmp/racer-stage31-{before,after,comparison,details,recovered}.json`,
`tmp/racer-stage31-observer.jsonl`, and the reboot request/pod artifacts.

## Exit health and next boundary

At12:07-12:08 UTC all1500 Nodes Ready, no true pressure conditions; all4500
DP/Gantry/loadgen pods Ready. All active1500-node DaemonSets Ready/Available;
other active deployments at desired readiness except the known intentional
leader-only Racer controller1/3. Stage30 documents deployed0c23 leadership
gating at `internal/racer/lifecycle.go:33,118-120`; no current-parent controller
source or image was substituted.

All1500 loadgen targets up and positive2m verified rates, concurrency6 on all.
Canary verified rate0.137GB/s versus control0.528GB/s. Pod-network DPf19,
operator d3/controller0c23, Gantryb8, loadgen208, catalog512 and all DS/deployment
specs/load-control data preserved. The single diagnostic pod was deleted
normally and its label query is empty; no tracing process remains on either
node (`tmp/racer-stage31-{final-health,audit,details,comparison}.json`).

The concrete new result rejects a normal guest reboot as a sufficient repair
under continuing C6. It does not prove that host/platform state was fully reset,
or identify whether the bottleneck persists outside guest state or is rapidly
recreated. Parent should retain the pod-network deployment and use this negative
canary, plus stage29/30 negative ring/channel results, for vendor-assisted
MANA/GDMA/platform investigation. No fleet underlay retry or additional reboot
is justified by this result. There is no demonstrated Racer source fix here.
