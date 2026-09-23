# Rust control plane implementation

## Accepted scope

Replace the Go control plane with conventional async Rust (Tokio, Axum/Hyper,
kube-rs, rustls, prost). Target 10,000 nodes. Fresh deployment and a versioned
wire/persistence cutover are acceptable. Keep the implementation small and tests
focused on observable integration behavior.

Topology is independently converging desired state: compile, persist immutable
content, CAS the committed pointer, publish. Nodes can skip revisions. No remote
prepare/receive/transmit/retire barriers or durable rollout/catch-up ledgers.
Ambiguous commits require authoritative readback. Preserve last-good state on
invalid inputs and failed local preparation.

Use /v4 control subscriptions with a received-state cursor separate from applied
revision/digest. Hold unchanged requests for 27-30 seconds, then respond normally.
Reconnect immediately on success; local reports and credential changes interrupt
polls. Operational observation freshness is 75 seconds. Proof freshness is a
separate security property.

Mixed-version peer requests use local placement. A hop budget travels through
HTTP and RDMA, is decremented at every forward, and is never reset by retry or
transport fallback. Bound candidate attempts and total execution separately.
Preserve universe, cache UID/generation, object integrity, and authenticated
membership. Avoid upstream-dependent coalescing for relayed requests and remove
resource-admission assumptions requiring a common acyclic topology.

Keep CA issuance/rotation separate: fenced leadership, durable-before-return
issuance, exact process identity, fresh TLS proofs, conservative retirement,
warm standby TLS. Keep storage policy independent and preserve exact quantity
semantics, invalid-input last-good behavior, and offer-bound feedback.

## Implementation phases and ownership

1. Core: standalone crate, deterministic topology, publication transitions,
   storage policy, integration contracts.
2. Adapters: Kubernetes inventory/persistence/runtime and native Rust security.
   Dataplane: independent application, long polling, bounded mixed-version routing.
3. Integration: production-binary tests, real Kubernetes/TLS, scale checks,
   build/container/CI/docs cutover, coverage-based Go retirement.

Subagents own distinct files and return verification results. The coordinating
agent integrates and commits verified milestones. Work takes place on
feat/racer-rust-controlplane in .worktrees/racer-rust-controlplane. Do not merge
into feat/racer-operator-integration before explicit user approval.

## Validation

Prefer integration scenarios over tests mirroring implementation. Cover commit
conflicts and uncertain outcomes, restart and stale leadership, independent node
convergence, long-poll races/cancellation, actual TLS issuance/rotation, HTTP/RDMA
cycles and admission saturation, volume isolation, and 10,000 idle subscribers.
Report unavailable runtime prerequisites separately from passing tests. Do not
claim hardware throughput from structural tests or fake Kubernetes clients.

Superseded Go code and tests are removed only after retained behavior has verified
replacement coverage. Shared Go SDK/operator helpers remain where needed.
