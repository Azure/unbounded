# Racer host-network operator release

## Configuration contract

The supported configuration is the preserved `racer-config` ConfigMap in the
operator namespace. Patch these three keys together in one API write:

```yaml
data:
  RACER_HOST_NETWORK: "true"
  RACER_PEER_PORT: "18082"
  RACER_DIAGNOSTICS_PORT: "19090"
```

These are proposed ports, not a claim that they are free on the fleet. Do not
activate host networking until every eligible node has passed the checks below.
The settings are mutable, but changes are not an atomic fleet transition and
are not safe under serving load.

- `RACER_HOST_NETWORK` accepts exactly `true` or `false`, default `false`.
- `RACER_PEER_PORT` defaults to 8082 and is shared by the existing controller
  publication and the operator-rendered peer listener.
- `RACER_DIAGNOSTICS_PORT` is optional. Omission preserves legacy 9090, or 9091
  if the peer port is 9090. Explicit diagnostics ports must differ from peer.
- Both explicit ports must be integers in 1024..65535. Explicit empty, zero,
  malformed, privileged, overflowing, or colliding values reject the operator
  plan. Previously accepted privileged peer ports are now rejected.
- Use `racer-config`, not `racer-dataplane-config` or generic Pod overrides.
  The latter does not coordinate controller publication. Do not override
  peer port, listener env vars, ports, DNS, or rollout strategy separately.

The existing ConfigMap preservation and hash mechanism consumes the settings;
no new API, RBAC, dependency, or global override exception is introduced.
Host networking changes only the dataplane Pod namespace and sets
`ClusterFirstWithHostNet` DNS. Controller Pods retain ordinary Pod networking.
The dataplane continues binding to downward-API `status.podIP`, including IPv6,
not wildcard addresses or an administrator-supplied Node IP. Token audience,
ownership checks, trust, identity, mounts, capabilities, and peer authentication
are unchanged. Host networking is not a request to enable plaintext transport.

## Source basis

At the deployed base `0c23d0e93338979d41b9f78568ea08f7587fa0bc`:

- `internal/operator/override/allowlist.go:149` protects host namespace membership;
  `internal/operator/override/validate_test.go:210-212` tests rejection.
- `internal/operator/components/racer/config.go:18-47` preserves administrator
  ConfigMap data. `racer.go:201-214,228-244,274-281` reads workload config, hashes
  controller config, and constructs the dataplane with tuning from a second map.
- `internal/racer/config.go:63-76` already reads `RACER_PEER_PORT` and ignores
  unknown workload-only keys. `topology_controller.go:140` passes that global
  port to membership reconciliation.
- `internal/racer/membership.go:127-153` requires current DaemonSet ownership and
  uses `status.podIP`; readiness is not an endpoint-selection prerequisite.
  `membership.go:210-220` retains the last endpoint across Pod gaps.
- `internal/racer/workload.go:98-103,135-152,173-192` uses zero surge, Pod-IP-bound
  listeners, named ports, and a named diagnostics readiness probe.
- `deploy/racer/controller.yaml.tmpl:9-16,29-31` uses three replicas, Recreate,
  and the same `racer-config` through envFrom.

The requested host transport change does not alter the invariants in
`~/design.md:19,21,39,56-59` (encryption, readiness-independent membership,
integrity, and control-plane identity/topology). Hardware offload and throughput
improvement require live measurement; this configuration alone proves neither.

## Release boundary and build components

Build and release **only `unbounded-operator`** from the release commit based on
the deployed base above. Keep the controller at its deployed image and keep the
compatible f19 dataplane image pinned. No controller or dataplane rebuild is
needed: the existing controller parses peer port and the existing dataplane
parses the two listener addresses. No parent protocol changes belong in this
image. Preserve all component image pins when updating the operator; do not
change a global image tag that implicitly upgrades other components.

## Quiesced rollout and publication transition

1. Export the live operator image and component pins, both Racer ConfigMaps,
   Deployment/DaemonSet, override configuration, Pod UIDs/IPs, and current
   publication sequence/membership. Never delete or restore durable identity,
   issuer, keyring, version, or installation state as part of this rollout.
2. Have operations inspect **all eligible nodes**, including system nodes and
   all IP families, for TCP listeners and reserved ports 18082/19090. Check
   wildcard bindings as well as Node-IP bindings. Existing 8082 conflicts are
   why the proposed peer port differs. Verify underlay routes, firewall/NSG
   access between peers, and diagnostics access from authorized monitors and
   kubelet. A free-port check is not a reservation: coordinate port ownership
   with node agents and repeat immediately before activation.
3. Host-network diagnostics are now reachable at a host address; do not assume
   Pod NetworkPolicy still confines them. Check the actual diagnostics API and
   require equivalent host firewall/NSG isolation before opt-in. Preserve peer
   authentication and restrict both ports to the intended sources.
4. Upgrade only the operator with all network settings unchanged. Verify no
   unexpected Racer rollout or image changes before continuing. Stop the
   rollout if generic protected overrides appear in the plan.
5. Quiesce **all clients/producers**, not just benchmark traffic, and drain
   in-flight work. Leave them stopped throughout the transition. Update all
   three keys in one write. Controller Recreate and dataplane rolling update
   are separate Kubernetes operations; this release does not pretend otherwise.
6. The new controller may publish `old Pod IP:18082` before that Pod listens on
   18082. Conversely an old controller may still publish 8082 for a new Pod.
   Stale admitted endpoints survive Pod gaps. Do not use readiness, a successful
   ConfigMap write, or one updated Pod as proof of a completed transition.
   Keep zero surge (one dataplane per node) and monitor rollout in bounded polls.
7. Before resuming traffic, require all desired dataplane Pods on the new
   template, no old/terminating dataplane Pods, correct host-network/DNS/listener
   settings and image pins, and every controller replica on the new config hash.
   Verify the leader's committed publication maps every member to its current
   owned Pod's `status.podIP:18082`; independently verify Pod IP equals the
   intended host address. Verify every dataplane has accepted the publication,
   diagnostics/readiness at 19090, and authenticated cross-node peer traffic.
   Update monitoring targets to the new named diagnostics port. Unexplained
   missing members, stale ports, unavailable nodes, or missing telemetry block
   resumption. Do not remove membership merely because a Pod is unready.
8. Run a bounded verified-data smoke check, then resume controlled traffic and
   compare useful NIC bytes, softirq, errors, and integrity under equivalent
   load. Roll back on integrity, authentication, node health, or persistent
   availability failures. Do not claim NIC saturation from this source release.

## Failure-closed recovery

Invalid settings return no executable plan; existing workloads remain unchanged.
This validation is not a fleet port reservation or a runtime rollback controller.
For rollout failure, stop/keep stopped all clients and producers first, preserve
diagnostics, and restore the **previous three configuration values** together
(delete keys that were previously absent). Keep the new operator until the
reverse transition has fully converged. Verify old Pod networking, listener
ports, controller config hash, committed endpoints and dataplane acceptance
before resuming. Only then, if necessary, restore the prior operator image.
Rolling the operator binary back first could ignore the opt-in while leaving a
custom controller peer port active. Do not force progress by relaxing admission,
global override guards, authentication, or membership ownership. Do not reset
publication counters or identity/storage to make rollback appear healthy.

Every command and poll must use external TERM timeout with kill-after 10 seconds,
at most 300 seconds. A timeout is a failed gate, not permission for an unbounded
retry. Fleet port validation, image publication, and cluster rollout are separate
operations gates; source tests cannot establish their success.
