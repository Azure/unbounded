# Updating Azure Container Linux Nodes from Nebraska

| Field | Value |
|---|---|
| Status | Draft for discussion |
| Scope | Automatic OS updates for Azure Container Linux (ACL) nodes, using Nebraska and `trident-acl-agent` |
| Audience | Unbounded maintainers, and the Trident and ACL teams |
| Repository baseline | `origin/main` at `1973dcf1a` |
| Date | 2026-11-18 |

## 1. Summary

ACL nodes update their operating system with an A/B update: the node keeps two
copies of the OS, writes the new version into the copy it isn't using, and
restarts into it. A program on each ACL node, `trident-acl-agent`, performs the
update when it is asked to through annotations on the node's Kubernetes Node
object.

Something in the cluster has to decide when each node updates, and ask it to.
This document proposes that piece:

1. `unbounded-acl-update`, one service in the cluster. For each ACL node, it
   asks Nebraska, the update server ACL uses, whether a new version is ready
   for that node. When one is, it moves the node's workloads to other nodes,
   asks the node's agent to install the new version, and checks that the node
   came back running it.
2. `unbounded-acl-update-node-info`, a small temporary DaemonSet. It publishes
   two facts about each node that the service needs, until `trident-acl-agent`
   publishes them itself.

The service never takes more than a set number of nodes in the cluster out of
service at once, one by default, so workloads always have somewhere to run. It
moves workloads with the same rules as `kubectl drain`, checks that a node can
be drained before starting its update, and pauses an update whose drain is
blocked rather than forcing pods off the node. Nebraska decides which version
nodes get, and how fast the whole fleet moves. When an update fails, the
service stops updating the cluster and waits for a person.

## 2. Background

### 2.1 Words used in this document

- **ACL**: Azure Container Linux, an OS image whose `/usr` is read-only.
- **A/B update**: an update that writes the new OS into a second copy (slot A
  or B), restarts into it, and keeps it only if it boots.
- **Trident**: the program on ACL that writes OS images to disk and switches
  between the two copies.
- **`trident-acl-agent`**: runs on each ACL host. It watches its Node object for
  update requests and runs Trident.
- **Nebraska**: an update server from the Flatcar project
  ([flatcar/nebraska](https://github.com/flatcar/nebraska)). It knows which OS
  versions exist and decides which machines may update now.
- **Update check**: a message a machine sends Nebraska that says who it is,
  which version it runs, and which track it follows, and asks for an update.
  Nebraska answers "no update" or describes an update to download. The format
  is the Omaha protocol.
- **Track and group**: a machine names a track in its update check. Nebraska
  maps the track to a group. Each group points at one version and has rollout
  settings, such as how many machines may update at a time.
- **Grant**: when Nebraska answers an update check with an update, it records
  that this machine is now updating. This is a grant.
- **Instance ID**: the ID a machine sends in its update checks. Nebraska keeps
  its records per instance ID.
- **Cordon**: mark a node so that Kubernetes schedules no new pods on it.
- **Drain**: move a node's pods to other nodes, by asking Kubernetes to evict
  them one at a time.
- **PodDisruptionBudget**: a rule an application sets, such as "keep at least 2
  of my pods running". Kubernetes refuses an eviction that would break it.
- **Place**: in this document, one of the slots that limit how many nodes the
  service updates at once (section 7).
- **nspawn machine**: on Unbounded ACL nodes, kubelet and its pods run inside a
  container on the host, started with `systemd-nspawn`. Pods see that
  container's files, not the host's.

### 2.2 How an ACL node updates

`trident-acl-agent` reads and writes three annotations on its Node, all under
one prefix that is configured on the node
([the annotation contract](https://github.com/microsoft/trident/blob/main/docs/Explanation/Trident-ACL-Agent.md#the-annotation-contract)):

- `update-request`: written by whoever drives the update. It asks for
  `stage`, `finalize`, or `rollback`.
- `update-status`: written by the agent. It reports how the request went.
- `update-commit-status`: written by the agent after the restart. It reports
  whether the node is running the new version.

An update goes like this:

1. The driver writes a `stage` request. It names the target version and the
   Nebraska server, app ID, and track to get it from.
2. The agent sends Nebraska an update check, and makes sure the version offered
   is the target version.
3. The agent downloads the image and writes it into the unused copy. It reports
   `Success` in `update-status`.
4. The driver writes a `finalize` request for the same update.
5. The agent tells Trident to start from the new copy next time, reports
   `Success`, and restarts the node.
6. After the restart, the agent confirms the node started from the new copy.
   Trident calls this commit. The agent reports `Success` in
   `update-commit-status`.
7. The agent tells Nebraska the update is complete.

```mermaid
sequenceDiagram
    participant Driver
    participant API as Kubernetes API
    participant Agent as trident-acl-agent
    participant Nebraska
    participant Trident

    Driver->>API: 1. write stage request (target version)
    API-->>Agent: the agent sees the request
    Agent->>Nebraska: 2. update check
    Nebraska-->>Agent: the update, with its download address
    Agent->>Trident: 3. write the new image into the unused copy
    Agent->>API: update-status: stage Success
    Driver->>API: 4. write finalize request
    Agent->>Trident: 5. start from the new copy next time
    Agent->>API: update-status: finalize Success
    Note over Agent,Trident: the node restarts
    Agent->>Trident: 6. commit
    Agent->>API: update-commit-status: Success
    Agent->>Nebraska: 7. update complete
```

Trident leaves the driver to the cluster. Nothing in Unbounded drives these
annotations today. This document adds the driver.

### 2.3 How Nebraska decides which machines update

1. A machine sends an update check with its instance ID, its current version,
   and its track.
2. Nebraska finds the group for the track. If the group's version is newer than
   the machine's, and the group's rollout settings allow it, Nebraska answers
   with the update and records a grant for that instance.
3. The grant counts against the group's limit until the machine reports the
   update complete, or until the group's update timeout passes. In safe mode,
   too many timed-out grants turn updates off for the whole group.
4. If the same instance asks again while its grant is open, Nebraska answers
   with the same update. It doesn't count it twice or check the settings
   again.

So the update check is what grants the update. Two things follow for anyone
who sends an update check on a node's behalf:

- It must send exactly what the node's agent sends: the same instance ID,
  version, track, and architecture. Otherwise Nebraska sees a second machine,
  and gives it a grant that never finishes.
- It must start the update right away after a grant, and finish it before the
  group's update timeout. A grant left waiting uses up the group's limit, and
  ends as a timeout.

The details are in Nebraska's `backend/pkg/api/runtime/updates.go`
(`GetUpdatePackage`).

### 2.4 What testing showed

The ACL update harness in AKSFlexNode
([Azure/AKSFlexNode#462](https://github.com/Azure/AKSFlexNode/pull/462),
`hack/acl-abupdate`) drives updates through `trident-acl-agent` with a test
Nebraska. These findings shape this design:

1. The agent's instance ID is the first 16 bytes of the SHA-384 hash of the
   host's `/etc/machine-id`, written as a UUID
   (`crates/osutils/src/machine_id.rs` in Trident). The harness confirmed it on
   a test node.
2. The agent ignores a request it can't parse, and writes no status for it
   ([microsoft/trident#957](https://github.com/microsoft/trident/issues/957)).
   That includes a request with any field the protocol doesn't define. The
   driver must write requests exactly to the schema, and must notice when no
   status appears.
3. If the node falls back to the old copy during the restart, the agent still
   reports the commit as a success
   ([microsoft/trident#956](https://github.com/microsoft/trident/issues/956)).
   The driver should check which version the node actually runs.
4. ACL's `VERSION_ID`, such as `3.0.20261104`, can be the same for two builds,
   so it can't be the version Nebraska compares. ACL images need a version
   that is different for every build.
5. On Unbounded ACL nodes, pods run inside the nspawn machine. They can't read
   the host's `/etc/machine-id` or `/etc/os-release`.

## 3. Goals

1. Update ACL nodes automatically when Nebraska has a newer version for them.
2. Never take more than a set number of the cluster's nodes out of service at
   once, and move workloads off a node before it restarts, without breaking a
   PodDisruptionBudget.
3. Let Nebraska decide which version nodes get, and how fast the whole fleet
   moves.
4. Never create extra Nebraska instances, or grants that a node won't use.
5. Notice failed updates, missing statuses, and nodes that don't come back, and
   stop the cluster's updates for a person.
6. Keep all state on the Node objects, so the service can restart at any time.

## 4. Not in the first version

1. Maintenance windows, beyond Nebraska's office hours setting.
2. Separate limits per node pool or zone. The first version has one limit for
   all the nodes it manages.
3. Rolling back automatically.
4. Choosing versions. Nebraska decides which version each group gets.
5. Replacing, or depending on, the HostUpgrade design
   ([host-upgrade.md](host-upgrade.md)). See section 17.

## 5. Overview

```mermaid
flowchart TB
    Nebraska[("Nebraska")]
    subgraph cluster["Kubernetes cluster"]
        Service["unbounded-acl-update<br/>2 replicas, 1 active"]
        API[("Kubernetes API<br/>Node objects")]
    end
    subgraph aclnode["Each ACL node"]
        Agent["trident-acl-agent<br/>on the host"]
        Info["unbounded-acl-update-node-info<br/>temporary DaemonSet"]
        Trident["Trident"]
    end

    Service -->|"1. update check, as the node"| Nebraska
    Service -->|"2. cordon, drain, update request"| API
    API -->|"3. the agent reads the request"| Agent
    Agent -->|"4. update check, download"| Nebraska
    Agent -->|"5. stage, finalize, commit"| Trident
    Agent -->|"6. statuses, agent-info"| API
    Info -.->|"agent-info, until the agent writes it"| API
```

There are three parts:

1. **`unbounded-acl-update`** is a Deployment in `unbounded-system` with two
   replicas. Leader election keeps one active. It watches the Nodes that carry
   its `managed` label. It decides which of them may update now, sends their
   update checks to Nebraska, moves their workloads, and writes their update
   requests.
2. **`trident-acl-agent`** ships with ACL and runs on each host. Unbounded
   configures it (section 15).
3. **`unbounded-acl-update-node-info`** is a temporary DaemonSet that writes
   each node's `agent-info` annotation (section 6).

### 5.1 Who controls what

| Control | Who | Why |
|---|---|---|
| How many of the cluster's nodes are out of service at once | the service: a limit, one node by default (section 7) | Workloads always have somewhere to run, however Nebraska is set up. |
| Moving workloads off a node before it restarts | the service: cordon, then drain with `kubectl drain`'s rules (section 9.1) | Pods move first, within their PodDisruptionBudgets. |
| Stopping after a failure | the service (section 10) | One failed node stops updates in this cluster until a person looks. |
| Which version, in which order of groups, at what pace across the fleet, and pausing | Nebraska | Releases are managed in one place, across every cluster. |

### 5.2 Where the code would live

| Part | Where | Follows |
|---|---|---|
| Service and DaemonSet binary | `cmd/unbounded-acl-update`, with `controller` and `node-info` subcommands | `cmd/token-refresher` (leader election with controller-runtime) |
| Shared code | `internal/aclupdate/...` | |
| Manifests | `deploy/acl-update/` | `deploy/token-refresher/`, `deploy/net/node/` |
| Admission policies | `deploy/acl-update/` | `deploy/net/controller/09-vap.yaml.tmpl` |
| Image | `images/unbounded-acl-update/` | `images/token-refresher/` |

## 6. Node information: `agent-info`

The service sends update checks as each node, so it needs two facts that only
the host knows:

1. The node's Nebraska instance ID.
2. The version the agent reports to Nebraska.

They go in one annotation on the Node,
`acl-update.unbounded-cloud.io/agent-info`:

```json
{
  "schemaVersion": "1.0",
  "machineId": "ebb9a2bb-9365-d34e-133f-8da0664e7366",
  "currentVersion": "20261105.1.0"
}
```

The rules:

1. It's written when the agent starts, before the agent finishes an update that
   was waiting for the restart. So by the time a commit status appears,
   `agent-info` describes the new boot.
2. `machineId` is the ID the agent sends to Nebraska, the hash, not the raw
   `/etc/machine-id`. systemd says to keep the raw ID private.
3. `currentVersion` is exactly the version the agent sends to Nebraska, read
   from the running OS. If the agent couldn't read it and would fall back to a
   placeholder, the field is left out, and the service doesn't send update
   checks for that node.

The service gets everything else it needs from the Node itself:

- The architecture, from the `kubernetes.io/arch` label. `arm64` is sent to
  Nebraska as `aarch64`, as the agent does.
- Whether the node restarted, from `status.nodeInfo.bootID`.
- The agent's version, from `tridentVersion` in its status annotations.

### 6.1 Until the agent writes it

Writing `agent-info` is a change to `trident-acl-agent`, which we need to agree
with the Trident team. Until it ships, the `node-info` DaemonSet writes the same
annotation:

1. Unbounded's host agent makes the host's `/etc/machine-id` and
   `/etc/os-release` readable, read-only, inside the nspawn machine.
2. The DaemonSet's pod reads both files through `hostPath` volumes.
3. It computes the instance ID the same way the agent does: the SHA-384 hash of
   the 16 bytes of the machine ID, cut to 16 bytes, written as a UUID.
4. It reads the version with the same settings the agent uses
   (`TRIDENT_ACL_AGENT_CURRENT_VERSION_PATH` and `_KEY`), which it gets from a
   ConfigMap.
5. It writes `agent-info` when it starts, and again whenever the values change.

The DaemonSet and the agent read the same files the same way, so they write
the same annotation. One difference: after a restart, the DaemonSet's pod may
start after the agent has already reported the commit. So the service waits
for `agent-info` to show the new version, rather than reading it once
(section 9). Once the agent writes `agent-info` itself, we stop deploying the
DaemonSet, and the service doesn't change.

## 7. How many nodes update at once

Updating a node takes it out of service: its pods move away, and it restarts.
If many nodes did that at once, their workloads would have nowhere to run. So
the service limits it in the cluster, whatever Nebraska allows.

### 7.1 Places

1. The service has a fixed number of places: the "nodes out of service at
   once" setting, one by default.
2. A node holds a place from just before its update check until its update is
   done, which includes waiting for its workloads to recover (section 9). A
   node whose drain is blocked gives its place back while it waits
   (section 9.1).
3. Nodes that are already out of service for other reasons use up places too:
   managed nodes that are not Ready, or that someone else cordoned. So the
   service never takes a node down while the cluster is already short.
4. While any managed node is held after a failure (section 10), the service
   gives out no places. Updates in the cluster stop until a person looks.
5. While as many nodes are drain-blocked as the "blocked nodes at once"
   setting, the service gives out no places either. Several blocked drains
   point to a wider problem than one workload.
6. Free places go first to drain-blocked nodes that can now be drained, since
   their updates are already under way. Then they go to nodes with the canary
   label, then to the node that was checked longest ago.

```mermaid
flowchart TB
    Start(["a place frees, or the check interval"]) --> Count["count free places:<br/>limit, minus nodes updating,<br/>minus nodes not Ready or cordoned"]
    Count --> Stop{"any node held, or too<br/>many drain-blocked?"}
    Stop -->|yes| Wait(["give out no places"])
    Stop -->|no| Free{"a free place?"}
    Free -->|no| Wait2(["wait for a place"])
    Free -->|yes| Pick["give it to the next node:<br/>drain-blocked nodes that can now be drained,<br/>then canary nodes, then the rest"]
    Pick --> Check["drain check, then update check (section 8)"]
```

### 7.2 Why a node takes a place before its update check

The update check is what grants the update (section 2.3). If a node were granted
an update and then had to wait for a place, Nebraska's grant would stay open
until it timed out, and in safe mode timeouts can turn updates off for the
whole group. Taking the place first means the service acts on every grant
right away.

When Nebraska has nothing new, the node gives its place back at once. So when
there's no update, a pass through the cluster is just a series of quick checks.

### 7.3 Nebraska's part

1. The two limits stack. A node updates only when it has a place and Nebraska
   grants it an update.
2. Nebraska's group settings still apply:
   - Tracks give rings. Put canary nodes on their own track, and release a
     version to that group's channel first.
   - "Max updates per period" sets the pace across every cluster that uses the
     track.
   - Safe mode lets one machine try a new version first.
   - "Updates enabled" pauses the group, and office hours limit updates to
     working hours.
3. If Nebraska refuses an update because a limit is reached or updates are
   off, nothing was granted. The node gives its place back and is checked again
   at the next interval.
4. The group's update timeout must be longer than the release deadline plus
   the restart deadline (section 12): 1.5 hours with the defaults, so we
   suggest 2 hours. A grant then always ends, with the update finished or
   released (section 9.1), before Nebraska would count it as timed out.

## 8. Checking for updates

1. Each managed node is due for a check once per check interval (one hour by
   default). Whenever a place is free, the service gives it to the next node
   that is due (section 7).
2. It skips the node if any of these is true:
   - The node is paused.
   - The node is held after a failed update.
   - The node is drain-blocked; it is retried as section 9.1 describes.
   - Its `agent-info` is missing, or has no `currentVersion`.
3. Before asking Nebraska, it checks that the node could be drained now: the
   drain rules of section 9.1, and a dry-run eviction of each pod the drain
   would move. A dry-run eviction asks Kubernetes whether the eviction would be
   allowed, without evicting. If any pod couldn't be moved, the node gives its
   place back, and the service records an Event naming the pods and why. No
   update has started, so nothing in Nebraska is left open.
4. It sends Nebraska one update check as the node: the node's instance ID and
   version, its architecture, and the server, app ID, and track the request
   will name. It never sends progress events or "ping" messages; those belong
   to the agent.
5. If Nebraska answers "no update", the node gives its place back. Nebraska now
   lists the node with its current version.
6. If Nebraska answers with version V, it has granted V to the node. The
   service writes the `stage` request right away (section 9).
7. If Nebraska answers with an error, such as "updates disabled" or "limit
   reached", nothing was granted. The node gives its place back.

```mermaid
sequenceDiagram
    participant Service as unbounded-acl-update
    participant API as Kubernetes API
    participant Nebraska

    Service->>Service: 1. give a free place to the next node that is due
    Service->>API: 3. drain check: drain rules, dry-run evictions
    alt a pod couldn't be moved
        Service->>API: record an Event, give the place back
    else the node could be drained
        Service->>Nebraska: 4. update check as the node (instance ID, version, track)
        alt no update, or an error
            Nebraska-->>Service: no update
            Service->>Service: 5. give the place back
        else update to version V
            Nebraska-->>Service: version V (granted to this node)
            Service->>API: 6. write stage request for V
        end
    end
```

## 9. Running an update

1. The service writes a `stage` request for version V, with a new
   `nodeUpdateId` and `operationId`. It records the update in its `state`
   annotation. The node keeps running its workloads while it downloads.
2. The agent sends its own update check. Nebraska returns the same grant. The
   agent downloads V, writes it into the unused copy, and reports `stage`
   `Success`.
3. The service cordons the node, and records that it did.
4. The service drains the node (section 9.1). If the drain can't finish by the
   drain deadline, the update pauses as section 9.1 describes.
5. The service writes a `finalize` request with the same `nodeUpdateId`, a new
   `operationId`, and the same version.
6. The agent prepares the new copy, reports `finalize` `Success`, and restarts
   the node.
7. The node starts from the new copy. The agent writes `agent-info`, commits,
   writes `update-commit-status` `Success`, and tells Nebraska the update is
   complete.
8. The service waits until three things are true: the commit status is
   `Success` for this `nodeUpdateId`, `agent-info` says the node runs V, and
   the Node is Ready. If they aren't all true by the restart deadline, it holds
   the node (section 10).
9. The service uncordons the node, if it cordoned it.
10. It waits for the workloads it moved to recover: every Deployment and
    StatefulSet whose pods it evicted has all its pods available again, and the
    settle time has passed. Then it gives the place back, and the next node can
    start. The update is done.

Whenever a node leaves an update early, because a step failed or the node was
held, the service also uncordons it, if it cordoned it, and gives its place
back.

The service keeps watching while the node restarts. If the active replica runs
on the node being drained, the drain moves it, and the other replica takes over.

```mermaid
sequenceDiagram
    participant Service as unbounded-acl-update
    participant API as Kubernetes API
    participant Agent as trident-acl-agent
    participant Nebraska

    Service->>API: 1. stage request (V)
    Agent->>Nebraska: 2. update check (same grant)
    Agent->>Agent: download V, write the unused copy
    Agent->>API: stage Success
    Service->>API: 3. cordon the node
    Service->>API: 4. evict its pods (budgets apply)
    Service->>API: 5. finalize request (V)
    Agent->>API: 6. finalize Success
    Note over Agent: the node restarts into V
    Agent->>API: 7. agent-info (V), commit Success
    Agent->>Nebraska: update complete
    Service->>API: 8. check commit, version, and Ready
    Service->>API: 9. uncordon
    Service->>API: 10. wait for moved workloads, then give the place back
```

The service tracks each node in one of these states:

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle --> Checking: a place is free
    Checking --> Idle: can't drain now, no update, or Nebraska refused
    Checking --> Staging: Nebraska grants V
    Staging --> Draining: stage Success
    Staging --> Idle: AlreadyAtTarget, or stage failed
    Draining --> Finalizing: all pods moved
    Draining --> DrainBlocked: drain deadline passed
    DrainBlocked --> Draining: the node can now be drained, and a place is free
    DrainBlocked --> Idle: release deadline passed, grant released
    Finalizing --> Restarting: finalize Success
    Finalizing --> Staging: NotStaged
    Finalizing --> Idle: finalize failed
    Restarting --> Recovering: commit Success, runs V, Ready
    Recovering --> Idle: moved workloads recovered, settle time passed
    Restarting --> Held: boot failed, wrong version, or no answer by the deadline
    Staging --> Held: no status after 3 tries
    Finalizing --> Held: no status after 3 tries
    Held --> Idle: a person clears the hold
```

### 9.1 Moving workloads

Without a drain, Kubernetes moves pods off a node only after it has been
unreachable for 5 minutes, by default. A restart is often shorter, so the pods
would wait for the node and come back on it, out of service the whole time. So
the service moves them first.

The service drains with `k8s.io/kubectl/pkg/drain`, the code behind
`kubectl drain`, and uses kubectl's rules for which pods move:

| Pods | What the drain does |
|---|---|
| Static pods, which kubelet runs from files on the node | Skips them. |
| DaemonSet pods | Skips them, as `kubectl drain --ignore-daemonsets` does. They run on every node, so they stop and start again with the node. This is the one rule that differs from kubectl's default, which refuses to drain until told to ignore them. |
| Pods that keep data in `emptyDir` volumes, scratch space on the node | Refuses to drain the node, unless the "delete `emptyDir` data" setting is on (`kubectl drain --delete-emptydir-data`). The data is lost when the pod moves. |
| Pods with no controller, which nothing would create again | Refuses to drain the node, unless the "move pods with no controller" setting is on (`kubectl drain --force`). |
| Every other pod | Evicts it through Kubernetes' eviction API, so PodDisruptionBudgets apply, and gives it its own grace period to stop. An eviction a budget refuses is tried again until the drain deadline. |

The same rules, with a dry-run eviction of each pod, are the drain check that
runs before each update check (section 8). So most nodes that can't be drained
are skipped before any update starts.

#### When a drain is blocked anyway

Things can change between the drain check and the drain. For example, another
pod of the same application may stop being ready, so its budget refuses the
eviction. When the drain isn't done by the drain deadline:

1. The service uncordons the node, marks it drain-blocked in its `state`, and
   records an Event naming the pod and its budget. The update stays staged.
2. The node gives its place back. A blocked drain is a problem with one
   workload, not with the update, so other nodes continue.
3. Every 5 minutes, the service runs the drain check on the node again.
4. When the check passes and a place is free, the node gets that place ahead
   of nodes that haven't started an update (section 7.1). The service cordons
   and drains the node again, then finalizes the same update. The update
   continues at step 5 of section 9.
5. If the release deadline passes first, counted from Nebraska's grant, the
   service releases the grant. It writes a new `stage` request for the same
   version. Nebraska answers the agent's update check with "update in progress",
   so the agent tells Nebraska the update failed, which closes the grant, and
   reports `OperationFailed` (`trident-acl-agent`'s recovery for an update left
   in progress, in `annotations/orchestrator.rs`). The node keeps running its
   current version, goes back to Idle, and is checked again at later intervals.
   The staged copy is overwritten by its next update.

Releasing the grant before the group's update timeout means Nebraska never
records a timeout, which in safe mode could turn updates off for the group
(section 7.3).

```mermaid
sequenceDiagram
    participant Service as unbounded-acl-update
    participant API as Kubernetes API
    participant Agent as trident-acl-agent
    participant Nebraska

    Note over Service: the drain deadline passes
    Service->>API: 1. uncordon, mark drain-blocked, record an Event
    Service->>Service: 2. give the place back
    loop 3. every 5 minutes
        Service->>API: drain check
    end
    alt 4. the check passes, and a place is free
        Service->>API: cordon, drain, then finalize the same update
    else 5. the release deadline passes first
        Service->>API: new stage request (V)
        Agent->>Nebraska: update check
        Nebraska-->>Agent: update in progress
        Agent->>Nebraska: update failed (closes the grant)
        Agent->>API: stage OperationFailed
    end
```

#### Pods left on the node

After the drain, only DaemonSet pods and static pods are left on the node. They
stop when the node restarts. Kubelet's graceful node shutdown gives them time to
stop cleanly. Unbounded turns it on once we've confirmed it works with kubelet
inside the nspawn machine (sections 15 and 17).

### 9.2 What this means for applications

1. An application with two or more replicas and a PodDisruptionBudget stays
   available through updates. An application with one replica is out of
   service while its pod starts again on another node, once per update of its
   node.
2. Pods that keep data in `emptyDir` volumes, or that have no controller, stop
   their node from updating until an administrator turns on the matching
   setting (section 12). The service records an Event on the node that names
   them.
3. A PodDisruptionBudget that never allows an eviction, such as
   `maxUnavailable: 0`, stops the nodes running its pods from updating. The
   Event names the budget.
4. DaemonSet pods stay on the node, and restart with it.

## 10. When something goes wrong

| What happens | What the service does |
|---|---|
| `stage` reports `AlreadyAtTarget` | Marks the update done. The node already runs V. |
| `stage` or `finalize` reports `OperationFailed` or `AgentInternalError` | Leaves the update. The next check decides again. |
| A request gets `InvalidRequest` | Records an Event and leaves the update. This points to a bug in the request. |
| `finalize` reports `NotStaged` | Starts again with `stage`. |
| The drain check fails before the update check | Skips the node until the next interval, with an Event. No update starts. |
| The drain isn't done by the drain deadline | Marks the node drain-blocked and pauses its update (section 9.1). Other nodes continue. |
| A drain-blocked node can't be drained before the release deadline | Releases Nebraska's grant, and goes back to Idle (section 9.1). |
| As many nodes are drain-blocked as the "blocked nodes at once" setting | Starts no new updates until fewer are blocked. |
| The workloads it moved don't recover within 30 minutes | Keeps the place, so no other node starts, and records an Event. The place frees once they recover. |
| The commit reports `TargetBootFailed` | Holds the node. |
| The commit reports `Success`, but `agent-info` still shows another version at the restart deadline | Holds the node. This is the case in microsoft/trident#956. |
| No status appears for a request within 5 minutes | Writes the same request again. The agent treats a repeated request as one. After 3 tries, holds the node. |
| A status stays `InProgress`, but isn't refreshed for 5 minutes | Records an Event. If the step still isn't done by the step deadline, holds the node. |
| After `finalize`, the Node isn't Ready, or there's no commit status, within 30 minutes | Holds the node. |

Holding a node means:

1. The service stops sending update checks for the node.
2. It writes the reason, and the version that failed, in the node's `state`
   annotation.
3. It records a Kubernetes Event on the Node.
4. It uncordons the node, if it cordoned it, and gives its place back.
5. While the node is held, the service starts no new updates anywhere in the
   cluster (section 7.1).
6. A person looks at the node, then removes the `state` annotation to let the
   service continue.

The service holds the node instead of trying again because, after a failed
restart, Nebraska may already list the update as complete (microsoft/trident#956).
Nebraska would then offer the same version again, and the node would repeat the
same failed update.

A drain-blocked node isn't held. Its problem is a workload that can't move yet,
not the update, so the rest of the cluster keeps updating (section 9.1).

## 11. Leader election and restarts

1. The service runs two replicas. A Lease keeps one active.
2. All state is on the Node objects. A new leader continues where the old one
   stopped, including counting places, a drain under way, and drain-blocked
   nodes. Continuing a drain is safe: evicting a pod that's already gone does
   nothing.
3. The service writes every annotation change together with the Node's
   `resourceVersion`. If two leaders overlap for a moment, only one write
   succeeds, so a node never gets two different requests.
4. The two replicas should run on different nodes. A PodDisruptionBudget keeps
   one of them running while the service drains the node the other is on.

## 12. Settings

The service reads its settings from a ConfigMap:

| Setting | Default | Meaning |
|---|---|---|
| Nebraska URL | none | The `https` address of Nebraska's update endpoint. |
| App ID | none | Nebraska's app ID for ACL. |
| CA certificates | the image's | Certificates to trust for Nebraska. |
| Default track | none | The track for nodes without a track label. |
| Nodes out of service at once | 1 | How many places there are (section 7). |
| Check interval | 1 hour | How often each node is checked. |
| Status deadline | 5 minutes | How long to wait for a status before writing a request again. |
| Step deadline | 1 hour | How long `stage` or `finalize` may stay `InProgress` before the service holds the node. |
| Drain deadline | 30 minutes | How long one drain may take before the node's update pauses (section 9.1). |
| Release deadline | 1 hour | How long after Nebraska's grant a node may take to reach `finalize`. After that, the service releases the grant (section 9.1). |
| Blocked nodes at once | 2 | How many nodes may be drain-blocked before the service starts no new updates. |
| Restart deadline | 30 minutes | How long to wait for a node to come back after `finalize`. |
| Settle time | 5 minutes | How long to wait, after the workloads a drain moved have recovered, before the next node starts. |
| Delete `emptyDir` data | off | Whether the drain may move pods that keep data in `emptyDir` volumes, losing the data (`kubectl drain --delete-emptydir-data`). |
| Move pods with no controller | off | Whether the drain may evict pods that nothing would create again (`kubectl drain --force`). |

The annotation prefix is fixed: `acl-update.unbounded-cloud.io`.
`trident-acl-agent` on each node must use the same prefix (section 15).

## 13. Annotations and labels

| Key | Kind | Written by | Meaning |
|---|---|---|---|
| `acl-update.unbounded-cloud.io/managed` | label | Unbounded at join, or an administrator | The service updates this node. |
| `acl-update.unbounded-cloud.io/track` | label | an administrator | The Nebraska track for this node, instead of the default. |
| `acl-update.unbounded-cloud.io/canary` | label | an administrator | This node gets a place before the others. |
| `acl-update.unbounded-cloud.io/paused` | label | an administrator | Don't start new updates on this node. |
| `acl-update.unbounded-cloud.io/update-request` | annotation | the service | The request to the agent (Trident's contract). |
| `acl-update.unbounded-cloud.io/update-status` | annotation | the agent | The result of a request (Trident's contract). |
| `acl-update.unbounded-cloud.io/update-commit-status` | annotation | the agent | The result after the restart (Trident's contract). |
| `acl-update.unbounded-cloud.io/agent-info` | annotation | the agent, or the temporary DaemonSet | The node's instance ID and version (section 6). |
| `acl-update.unbounded-cloud.io/state` | annotation | the service | The node's update, when Nebraska granted it, whether the service cordoned the node, and why it's drain-blocked or held, if it is. |

An example `state`, for a node whose drain is blocked:

```json
{
  "phase": "DrainBlocked",
  "nodeUpdateId": "550e8400-e29b-41d4-a716-446655440000",
  "targetVersion": "20261106.1.0",
  "grantedAt": "2026-11-18T10:02:00Z",
  "cordoned": false,
  "reason": "evicting default/db-0 would break PodDisruptionBudget default/db",
  "since": "2026-11-18T10:42:00Z"
}
```

## 14. Permissions and safety

1. The service's account can:
   - get, list, watch, and patch Nodes;
   - list Pods, and evict them, including dry-run evictions;
   - get DaemonSets, ReplicaSets, Deployments, and StatefulSets, to apply the
     drain rules and to see when moved workloads have recovered;
   - create Events, and use a Lease for leader election.
2. Patching Nodes is a broad permission, so two admission policies narrow what
   can be written:
   1. The service's account may change only the `update-request` and `state`
      annotations, and whether the node is cordoned (`spec.unschedulable`). A
      request must name the configured Nebraska URL and app ID, so a stolen
      token can't point nodes at another server.
   2. The DaemonSet's account may change only `agent-info`, and only on its own
      Node. The policy checks the node name and UID in the pod's token, as
      `unbounded-net-node`'s policy does (`deploy/net/controller/09-vap.yaml.tmpl`).
3. Every pod the drain moves goes through Kubernetes' eviction API, so
   PodDisruptionBudgets always apply. That includes pods with no controller
   when the setting allows them, as with `kubectl drain --force`. The service
   never deletes pods directly.
4. The agent writes its statuses and `agent-info` with the node's own
   credentials, which can only change the node's own Node object.
5. The agent only accepts an `https` Nebraska URL, and only `https` or `oci`
   image addresses. It checks each image against a checksum from Nebraska.
6. The DaemonSet runs unprivileged, with two read-only files from the host.

## 15. Work needed elsewhere

**Trident (`trident-acl-agent`):**

1. Write `agent-info` (section 6).
2. Report a final status when Nebraska refuses an update during `stage`.
   Reading the code, the agent exits instead, and systemd restarts it; the
   status stays `InProgress` and the agent asks Nebraska again each time
   (`crates/trident-acl-agent/src/annotations/orchestrator.rs`, the
   `"Nebraska query failed"` error). The service makes this rare, since it only
   asks for `stage` after a grant, but it shouldn't happen at all.
3. Confirm that the recovery the service uses to release a grant is intended,
   and will stay: a new `stage` request while an update is in progress makes
   the agent tell Nebraska the update failed (section 9.1). It is in the code,
   with a comment saying why, but not in the contract's documentation. No code
   change is needed.
4. Fix microsoft/trident#956: a fallback to the old copy is reported as a
   successful commit.
5. Fix microsoft/trident#957: a request the agent can't parse gets no status.

**Unbounded's host agent:**

1. Configure `trident-acl-agent` on ACL hosts with a systemd drop-in:
   - the annotation prefix, `acl-update.unbounded-cloud.io`;
   - kubelet's credentials, and `kubelet-cert` to find the Node's name;
   - where to read the version (see the ACL images item).
2. Make the host's `/etc/machine-id` and `/etc/os-release` readable inside the
   nspawn machine, for the temporary DaemonSet.
3. Set the `managed` label when the node joins, if we choose to.
4. Turn on kubelet's graceful node shutdown, once we've confirmed it works with
   kubelet inside the nspawn machine (section 9.1).

**ACL images:**

1. Carry a version in `/etc/os-release` that is different for every build and
   matches the versions in Nebraska. Trident's documentation uses
   `IMAGE_VERSION` as its example.

**Nebraska:**

1. Run a release with package labels. 4.0.3 has none, and the agent reads the
   image's checksum from a label by default.
2. Keep every server behind the URL on one database, so the service's grant
   and the agent's update check are seen by the same Nebraska.
3. Set each group's update timeout longer than the release deadline plus the
   restart deadline (section 7.3).

## 16. Testing

1. Unit tests:
   - the state machine, with a fake Kubernetes client and a fake Nebraska;
   - counting places: nodes that are updating, recovering, not Ready, or
     cordoned by someone else, and no places while a node is held or too many
     are drain-blocked;
   - the drain check and the drain: static, DaemonSet, `emptyDir`, and
     no-controller pods with each setting, evictions a PodDisruptionBudget
     refuses, and the drain deadline;
   - a blocked drain: retrying, finalizing the same update, and releasing the
     grant at the release deadline;
   - the request format, against the JSON schemas in Trident's
     `annotations/protocol.rs`;
   - the instance ID: `/etc/machine-id` `34a267d412544d6a9e97b5ba6762f6e7`
     gives `ebb9a2bb-9365-d34e-133f-8da0664e7366`;
   - the admission policies' rules, as
     `internal/operator/components/net/vap_test.go` tests unbounded-net's.
2. The AKSFlexNode harness: its test Nebraska and annotation scenarios, with
   the service running against the harness's cluster. The harness's nodes run
   kubelet inside an nspawn machine, so it also checks:
   - a staged update survives a pause, a restart of the agent, and an
     unrelated restart of the node, and then finalizes;
   - graceful node shutdown with kubelet inside the nspawn machine.
3. Unbounded's ACL end-to-end tests (`agent-e2e-kind`, host `acl`), with
   several ACL nodes and a workload with a PodDisruptionBudget, to show that
   only one node is out of service at a time and the workload keeps running.
4. A real Nebraska, to confirm:
   - the agent's update check after the service's gets the same grant, and no
     extra instances appear;
   - releasing a grant with a new `stage` request makes Nebraska record the
     update as failed, not timed out, and a later update succeeds.

   Trident's own tests run a real Nebraska the same way
   (`tools/storm/aclagent`).

## 17. Open questions

1. **HostUpgrade.** [host-upgrade.md](host-upgrade.md) proposes driving Trident
   from Unbounded's host agent instead. Both can't drive the same node. Should
   this be a separate path for ACL nodes that use `trident-acl-agent`, or should
   the service create HostUpgrade operations instead of writing annotations?
   host-upgrade.md expects "the caller or a separate rollout controller" to
   drain nodes, which this service now does.
2. **`agent-info` in Trident.** Will the Trident team add it, under this name
   and format?
3. **ACL's version.** Which os-release key will carry the per-build version?
4. **The host's os-release.** Does `systemd-nspawn` on ACL already show it at
   `/run/host/os-release`?
5. **Graceful node shutdown.** Does it work with kubelet inside the nspawn
   machine?
6. **The `managed` label.** Should Unbounded set it when the node joins, or
   should administrators?
7. **Later versions.** Separate limits per node pool or zone, and maintenance
   windows: in this service, or in a shared rollout controller?

## 18. Other approaches we considered

1. **Letting Nebraska alone limit updates.** Its limits are shared by every
   cluster that uses a track, can be set loosely, and know nothing about
   workloads. A cluster could lose all its nodes at once.
2. **Taking a place after the grant.** Granted nodes would wait for places with
   their grants open, and those grants would time out.
3. **Draining before the update check.** No grant would be open while a drain
   waits. But it would move workloads off every node it checks, even when
   there's no update.
4. **A `cancel` operation in `trident-acl-agent`.** Not needed: the agent's
   existing recovery releases a grant (section 9.1).
5. **Forcing pods off after a deadline, ignoring their budgets.** It breaks the
   promise a PodDisruptionBudget makes to its application.
6. **Annotations from other tools to protect pods.** It would tie the service
   to those tools. PodDisruptionBudgets are Kubernetes' own way for an
   application to say how many of its pods may be moved.
7. **Running the `kubectl` binary for drains.** It behaves the same, but the
   service would parse text output and ship a second binary. The Go package
   gives the same rules with structured results.
8. **A DaemonSet that checks for its own node.** It could read the host's files
   directly. But a pod restarts with its node, so it can't notice a node that
   doesn't come back. Each pod would need its own narrow permissions, and
   decisions about the whole cluster, such as places and draining, would have
   nowhere to live.
9. **An instance ID made up by the service.** Nebraska would count every node
   twice. The made-up instance's grants would never finish, and in safe mode
   their timeouts can turn updates off for the group.
10. **Reading the group's version from Nebraska's admin API.** Only the agent's
    update check would then ask for the update. But the admin API needs a
    login, and when Nebraska refuses an update, the agent currently exits
    instead of reporting it (section 15).
11. **A fixed target version in the cluster's settings.** This is simple, and
    is how Trident's example works (a track named `pin-<version>`). But someone
    has to change the setting for every release.
12. **Using `VERSION_ID` as the version.** Several builds can share it.
