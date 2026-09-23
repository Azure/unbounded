<!-- Copyright (c) Microsoft Corporation. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Racer forward-recovery payload capacity

Forward recovery preserves exact historical snapshots for selected Pods that
have not acknowledged retirement. Unknown boots remain obligations. Retirement
of one boot does not prove that another boot has no handoff state. Proposed Pod
replacement cannot collect history before the replacement topology commits.

The previous 256 MiB aggregate payload limit could reject a correction even when
its metadata and first-boot reservations fit. Production caches have 262144 slots;
1500-node snapshots are much larger than fixtures with only 1500 slots. A real
262144-slot fixture with 228 unresolved recipients exceeds 256 MiB.

## Bounded storage and memory

The aggregate limit is now 1 GiB of distinct immutable snapshot bytes per universe.
This accommodates the metadata envelope of 256 new wildcard decisions at roughly
2.4 MiB each, with headroom. Individual snapshots remain limited to 64 MiB. The
ledger remains limited to 256 KiB and 512 entries, including reserved first-boot
bindings and grants. Wildcards and bound boots referencing the same digest share
payload accounting. Larger configurations or accumulated unresolved histories
still apply backpressure rather than discard necessary handoff state.

Planning preflights all payload references and removal reservations before any
chunk writes. It then regenerates and verifies one snapshot at a time, writes its
immutable chunks, and releases its body before constructing the next. The caller
holds the existing topology/subscription lock throughout. The ledger CAS follows
all chunk writes and precedes topology commit. Failed staging leaves only reusable
orphans; ambiguous writes invalidate the rollout until an uncached reload.

Topology GC requests ConfigMap metadata in pages of 100 instead of loading every
retained payload into memory. It still protects all chunks referenced by the
durable forward ledger, current/previous topology, and serving manifest. A GC
failure cannot undo a committed topology.

## Operational implications

This change uses the existing persisted references and chunk layout. No reset or
migration is required. The storage budget is compiled in, not a runtime flag.

The capacity fix does not replace retirement acknowledgments. In a universe with
about 1500 survivors, an empty ledger still cannot admit a correction that needs
over 256 new wildcard decisions. Retained history further reduces the available
admission headroom. This does not deadlock Pod-replacement recovery: terminal
delivery for the current revision precedes successor admission.

### Revision-29 phase-2 recovery

At phase 2, no correctly reporting worker can acknowledge phase 4 yet. Merely
deploying a PKI fix does not revive a boot already tombstoned by the old controller.
Five ID-only placeholders do not count as missing selected targets: only members
with an IP enter that test (`cmd/racer-controlplane/rollout.go:269-276`).

Replacing the affected selected Pods changes that situation:

1. Deploy the fixed controller and verify its serving leader. Confirm the exact
   affected Pod UIDs before replacement; preserve all durable state and slab files.
2. Replace the affected selected Pods through normal Kubernetes termination. Do
   not wait for each replacement to become Ready before addressing the other
   known affected Pods. New Pod UIDs initially wait for topology selection.
3. On observing any missing old selected UID, `rolloutBusy` durably commands
   **revision 29, phase 4, recovering=true**, before attempting successor admission
   (`rollout.go:286-299`). The phase change does not allocate a forward payload
   ledger. An over-capacity successor attempt leaves this terminal decision intact.
4. The surviving original Pods receive revision 29 / phase 4 through the normal
   authenticated handler (`rollout.go:544-550`). Rust allows a monotonic jump to
   phase 4 but still requires local preparation, reception, activation, and drain
   before acknowledging retirement (`src/control/activation.rs:61-69,131-178,208-226`
   under `cmd/racer-dataplane`). It does not require a successor to do that.
5. After survivors report phase 4, the planner omits their new handoff snapshots
   (`rollout.go:1155-1168`). Replaced Pod UIDs are not new handoff recipients.
   Existing unknown-boot obligations for still-selected Pods remain protected;
   post-commit collection removes only obligations inaccessible under the committed
   selection (`rollout.go:1237-1249`).
6. Observe successor publication, enrollment of replacements and placeholders,
   then ordinary prepare / receive / activate / retire completion. Failed or
   unauthenticated survivors still require diagnosis; Ready alone is not a
   retirement acknowledgment.

Deleting all 35 identified affected Pods leaves 1460 authenticated survivors, not
1460 permanent historical payload obligations. This recovery is bounded by current
topology delivery plus retained history; it does not require enlarging the ledger
to hold the whole fleet. If the workload has unrelated live, unreachable receivers
that cannot retire, the existing finite history limit still applies.

A same-Pod process restart can instead obtain a fresh random boot nonce and enroll
under the already selected Pod UID (`control.rs:527-535`, `enrollment.go:163-178`).
Once all selected processes report receive readiness, normal phase progression can
finish revision 29. This does not revive the old boot or collect any old live-Pod
PKI members: the fixed retirement rule keeps them until Pod UID absence. Prefer
Pod replacement for the concrete recovery above, which also supplies that absence.

`TestForwardCapacityProductionGeometry` covers realistic slot/node geometry,
admission failure before writes, uncertain chunk creation and retry, restart,
exact historical replay, and preservation of unknown-boot obligations.
`TestForwardStorageBoundsAndIntegrity` checks payload and metadata boundaries;
`TestStateGCMetadataPagination` checks continuation, retention, and interrupted GC.
`TestForwardCapacityPhaseTwoPodReplacementRecovery` models 1500 nodes, 1495 selected
phase-2 receivers, 35 replacements, and five placeholders. It verifies capacity
failure before replacement, durable current-revision terminal recovery despite
successor rejection and controller restart, handler-driven retirement of all 1460
survivors, successor publication, full-fleet normal barriers, and retention of an
unrelated unknown-boot obligation. Workers and Kubernetes are simulated; this is
not evidence that live workers have actually drained.
