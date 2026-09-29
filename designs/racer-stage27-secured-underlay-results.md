# Stage27 secured underlay operational result

Date: September 29, 2026. This is an experiment record, not a deployment recipe.

## Decision

Host-network C6 was rejected. No C8 or repeated window was run. Restored the
original pod-network dataplane template, peer8082, diagnostics9090, monitoring
configuration, and C6. Retained dataplane f19, controller0c23, Gantryb8, and
loadgen208. Operator d3f82a0f remains installed with host networking disabled.

Final recovery audit at11:19:40UTC: all1500 nodes and4500 benchmark pods Ready,
all1500 appliedC6 and positive verified-byte rates. A brief two-minute recovery
observation was569.866446GB/s at99.209640% success, not another acceptance window.

The single five-minute window, 10:52:23.818800 to10:57:23.818800UTC, measured:

| Metric | Result |
| --- | ---: |
| Fully verified goodput | 684.634460 GB/s |
| Aggregate pull success | 98.859972% |
| Nodes with positive verified goodput | 1489/1500 |
| Host eth0 TX / RX | 13.938626 / 13.928181 Tb/s |
| Mean host / dataplane CPU | 4.769307 / 2.983654 logical cores |
| Mean dataplane working set | 10.177233 GB |
| Counter resets / node identity or boot changes | 0 / 0 |

Coverage was complete, but success was below99%, eleven nodes had zero verified
goodput, and one dataplane had a readiness transition. Higher aggregate goodput
does not make this an accepted improvement. NIC saturation was not established.
The prior accepted Stage25 C6 remains587.395362GB/s at99.379563% success.

## Security experiment

A task-owned INPUT chain guarded only TCP18082/19090 at each exact IPv4 NodeIP.
Peer sources were an exact1500-entry hash:ip set. Diagnostics permitted the local
NodeIP and actual Prometheus sources (PodIP and observed SNAT NodeIP). All other
sources were rejected. Trusted traffic returned to preexisting policy; no broad
ACCEPT, chain flush, conntrack flush, identity operation, or host payload write
was used. A no-token capability-scoped DaemonSet maintained rules, and a dataplane
init gate prevented startup before local verification after a reboot.

The canary verified trusted peer/local connections, real Prometheus HTTP access,
and unauthorized-source rejection. All1500 nodes subsequently passed exact-set,
rule, free-port, IPv4-only binding, and readiness checks. The reconstructed
NodeIP:18082 membership hash matched committed membership4927/sequence4928.

The rule set was fully reversed after restoring PodIP:8082 membership. On all1500
nodes, the two new ports had no listener before removal; only task-owned jump,
chain rules, chain, and set were removed. Preexisting INPUT rules were verified
unchanged. The guard DaemonSet and ConfigMap were deleted.

## Actionable findings

- All eleven zero-goodput nodes were in ddsv6. Their local guard reject counters
  were zero while substantial peer traffic passed. Post-window failure-ring
  samples showed PeerReceiveBody/PeerRelay deadlines and candidate retries, with
  some plaintext/ciphertext admission pressure. These samples are not root-cause
  proof or complete unique request counts.
- Existing SSA ownership retained old numeric container-port entries during the
  transport update, causing duplicate named ports and rejecting the DaemonSet.
  A complete resourceVersion-checked template replacement resolved it without
  an intermediate partial transport template.
- The operator upgrade refreshes informational RACER_DATAPLANE_IMAGE wiring,
  changing the controller config hash even with the actual dataplane pinned.
  Operator-only upgrade was not controller-rollout-free.
- Controller availability was already1/3 before the experiment and remained1/3.
  All replicas used the intended config, but healthy three-replica HA is not
  claimed. Diagnostics expose usable membership, not each process's exact
  accepted publication sequence.
- The00007r runtime-only CPU6/7 quarantine was reapplied through both dataplane
  rolls. It remains a temporary nonuniform-CPU exception and does not survive
  another process restart without reapplication.

## Evidence and implementation basis

Local detailed report: `tmp/racer-stage27-secured-underlay.md`. Evidence includes
`racer-stage27-{canary-probe,fleet-guard,live-verification,membership,c6-summary,
c6-raw,c6-detail,failure-evidence,recovery-membership,guard-removal}.json` and
`racer-stage27-c6-per-node.csv`. Operational helpers are retained under
`tmp/racer-stage27-tools/`. No application build or source tests were repeated.

Relevant source contracts: `internal/racer/workload.go:150-156` binds listeners
to PodIP; `internal/racer/membership.go:108-112` defines endpoint ownership;
`internal/racer/wire/canonical.go:81-106` defines membership hashing;
`internal/operator/components/racer/config.go:34-44` overwrites owned wiring.
Live evidence, not these source contracts, establishes the observations above.
