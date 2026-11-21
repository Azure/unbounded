# MachineOperation Selectors

A `MachineOperation` can target one Machine by `spec.machineRef` or a group of
Machines by `spec.machineSelector`. This page describes how a selector
operation is carried out: one controller turns it into one operation per
machine and paces them. The operation kinds and who executes each are in
[Lifecycle Operations](lifecycle-operations.md).

## Problem

Operators need to act on a group of machines at once, for example to upgrade
the agent on every machine in a rack, without creating and supervising one
operation per machine by hand.

Before this design each executor resolved a selector on its own, and they
disagreed:

- Agents matched the selector against Node labels, falling back to Machine
  labels. Provider controllers matched Machine labels. The same selector
  could mean different machines depending on the operation kind.
- Every matching agent started at once. There was no way to work through a
  rack one machine at a time, or to stop after a failure.
- A selector that matched nothing, or a machine whose agent was down, left
  the operation unfinished forever with nothing to say why.
- Every agent watched every `MachineOperation` in the cluster, and results
  for all machines were written into one object. Neither holds up at the
  size of a large site.

## Goals

- One meaning for a selector, whichever component executes the operation.
- A configurable pace: one machine at a time, a number, a percentage, or all
  at once.
- A defined outcome for every machine and for the group, including machines
  that nothing serves.
- Work for 30,000 machines in one operation without an object or a watch
  whose size grows with the fleet.

## Non-goals

- Defining the labels to select on. Any Machine label works. A topology label
  scheme is a separate proposal.
- Ordering by anything other than machine name, health gates between
  machines, or automatic rollback.
- Selectors for `HostAllocate` and `HostRelease`, which target an allocation.
- Restricting which machines' operations a node's agent may report on.

## Behavior

A selector operation is a **parent**. `machina-controller` runs the fan-out
controller (`pkg/machineops/fanout`), which is the only component that acts
on a parent:

1. **Resolve.** It matches the selector against **Machine** labels and
   creates one **child** operation with `spec.machineRef` for every matching
   Machine. A selector that matches no Machine fails the parent at once with
   reason `NoMachinesMatched`.
2. **Hold.** A child that may not run yet is created with
   `spec.suspend: true`. Nothing executes a held operation, and a held
   operation does not make other operations on its machine wait.
3. **Release.** The controller clears `spec.suspend` on held children, lowest
   machine name first, so that at most `spec.rollout.maxConcurrent` children
   are released and unfinished.
4. **Finish.** The parent is `Complete` when every child completed, and
   `Failed` otherwise. A finished parent never changes again.

Agents, provider controllers and metalman execute only `machineRef`
operations that are not held. They treat a child like any other operation and
never look at a parent.

```text
parent (machineSelector, rollout)
  |  machina-controller: resolve, hold, release, count
  +-- child (machineRef: machine-a)            released -> agent or provider controller
  +-- child (machineRef: machine-b, suspend)   held until its turn
  +-- child (machineRef: machine-c, suspend)   held until its turn
```

### Failure

`spec.rollout.onFailure` decides what a failed child means:

- `Stop` (default): no further children are released. The ones already
  running finish, and the parent ends `Failed` with reason `RolloutStopped`.
  Children that were never released never run.
- `Continue`: the rest still run, until `spec.rollout.maxFailures` children
  have failed. Then the rollout stops as above, with reason
  `FailureLimitReached`. If every child finishes before that, the parent
  ends `Failed` with reason `TargetFailed`.

`maxFailures` defaults to 1000, which is also the most allowed. An operation
on 100,000 machines where everything fails stops after about a thousand
instead of working through all of them. Children already released when the
limit is reached still finish, so more than `maxFailures` can fail.

Two more cases count as a failed child:

- **Nothing takes it.** An agent writes `Pending` as soon as it takes a
  child. A released child that nothing has taken for 5 minutes fails with
  reason `NotPickedUp`, provided no other child of the same parent was taken
  in that time either, so a controller working through a long queue is not
  mistaken for an absent one.
- **It is deleted** after the set of machines is fixed.

A child that has been taken is never timed out. If its machine dies
mid-operation, the parent waits until someone deletes that child.

### Which machines

The set is fixed once every child exists (condition `TargetsResolved`). Until
then a Machine that starts to match is included. Afterward, new Machines are
not added and deleted children are not recreated.

### Retention

The parent's `status.summary` is the durable record of a selector operation.
Children exist to do the work and to show what went wrong:

- A **completed** child may be removed at any time after it completes. Today
  that happens when the parent finishes, because the controller counts by
  listing children. Nothing may rely on a completed child staying around.
- A **failed** child is kept until the parent is deleted, so the failure can
  be investigated. A finished parent keeps at most 1000 of them, the first by
  machine name; the summary still has the full count.
- A **held** child of a stopped rollout is removed when the parent finishes.
  It carries nothing but a machine name, and the summary keeps the count.
- Nothing may rely on a child existing for a machine before its turn.

So a finished parent leaves at most 1000 children behind, however many
machines it matched. Two exceptions: `spec.rollout.childRetention: All`
keeps every child, and a child that carries a boot media record is always
kept, because metalman reads it. Deleting the parent removes all its
children.

The weak promises are deliberate. They leave room to remove completed
children during a rollout, or to create children a window at a time, without
an API change.

## API

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-rack-01
spec:
  operationKind: AgentUpgrade
  machineSelector:
    matchLabels:
      example.com/rack: rack-01
  rollout:
    maxConcurrent: 25%        # integer >= 1, or 1% to 100%. Default 1.
    onFailure: Continue       # Stop (default) or Continue.
    maxFailures: 20           # With Continue: 1 to 1000. Default 1000.
    childRetention: Failed    # Failed (default) or All.
status:
  phase: InProgress
  message: "machines: 40 total, 26 held, 10 active, 4 complete, 0 failed"
  summary:
    total: 40
    held: 26
    active: 10
    complete: 4
    failed: 0
    failedMachines: []        # at most 10 names
```

- Exactly one of `machineRef` and `machineSelector` is set. `rollout` is
  allowed only with a selector.
- The spec of a selector operation is immutable. `machineRef` is immutable.
- `spec.suspend` is allowed only on a `machineRef` operation, can be set only
  at creation, and can only be cleared afterward.
- The parent's status holds counts only. The children are the per-machine
  record.
- A child is named `<parent>-<machine>-<hash>`, is owned by the parent, and
  carries the label `unbounded-cloud.io/parent-operation-uid`.
- `spec.machineRef` and `status.phase` are selectable fields, so clients can
  list operations for one machine or in one phase.

## Access control

- `machina-controller` may create, patch and delete `MachineOperation`
  objects and update their status.
- Agents may read operations and update their status. They may not delete
  operations: no agent does, and a deleted child counts as a failure.
- An admission policy allows the status of a parent, and of a held child, to
  be written only by an identity that may create `MachineOperation` objects.
  Without it a node could mark a group finished, forge its counts, or write a
  result onto a held child before its turn.

## Scale

The design keeps every per-machine cost constant:

- The parent's size does not depend on the number of machines.
- Each agent watches only the operations with its own `spec.machineRef`.
- The controller creates and releases children in batches with its own
  rate-limited client, and reconciles a parent at most about once a second
  however many children report.

Measured on a small managed cluster with 30,000 machines released all at
once: complete in about 10 minutes with no errors, a 1.6 KB parent, about
5 KB received per agent watch, and all completed children removed in about
5 minutes afterward. Time grows linearly with the number of machines, because
children are created at 50 to 100 per second.

`machina-controller` caches one operation per machine while a group
operation runs, so its memory grows with the fleet.

## Limits and open questions

- Selector operations need `machina-controller`. An install that runs only
  a provider controller or only metalman executes `machineRef` operations
  and leaves a selector operation untouched.
- A parent whose taken child never finishes waits until that child is
  deleted. There is no per-operation deadline.
- While a rollout runs, every matched machine has a child object. The
  bound on children applies once the parent has finished.
- With `maxConcurrent: 100%` every child is released at once, so
  `maxFailures` cannot stop anything and all of them may fail.
- A failure reported after a completed child was removed is lost. This can
  happen when an agent upgrade reports success and is rolled back afterward.
- A node's agent can still write the status of another machine's released
  operation. Limiting that needs per-node authorization.
- An agent, provider controller or metalman older than this design does not
  know `spec.suspend` and would run a held child. All components must be
  upgraded together.
- Whether the fan-out controller should become its own component, or be
  reused by provider controllers, is open. The package does not depend on
  `machina-controller`.
