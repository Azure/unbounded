# Partitioning Gantry by Node Labels

**Status:** Draft for discussion

## Summary

Gantry normally operates as one peer network across a Kubernetes cluster.
Every agent can discover content held by any other agent, and a cold image
object is coordinated across the whole cluster.

This design allows an operator to divide that peer network into independent
groups using an arbitrary Kubernetes Node label. The same mechanism can group
nodes by availability zone, rack, hardware pool, network boundary, or an
operator-defined placement policy.

For example:

```yaml
topology.kubernetes.io/zone: eastus-1
```

or:

```yaml
gantry.unbounded-cloud.io/group: gpu-rack-17
```

Each distinct label value becomes a Gantry peer group. Members of a group use
their own peer-discovery network and coordinate origin pulls only with one
another. There is no configured group count and no requirement that groups be
equal in size.

An existing cluster can be partitioned with one ordinary Gantry DaemonSet
rollout. As each Pod is replaced, the new agent joins only its label-derived
group. Old agents remain in the cluster-wide network until they are replaced.
The two populations do not discover one another during the rollout.

This temporary separation is acceptable. It can reduce peer-cache hits and
produce duplicate origin pulls, but image pulls continue through Gantry's
normal origin fallback. Node-local content remains in containerd across the
Pod replacement and is advertised into the new group.

Gantry does not watch all Nodes to maintain these groups. Each agent reads only
the Node on which it runs when it starts.

## Gantry Concepts

In this document, a **peer network** is the set of Gantry agents that discover
and exchange image content with one another. It is not a separate Kubernetes
Pod network.

Agents use a distributed hash table (DHT) as the peer-to-peer directory. A
**provider record** says that one agent has a particular image object. If no
provider exists, a **cold puller** is the agent chosen to fetch that object from
the origin registry. A **Lease slot** is a precreated Kubernetes Lease object
that publishes a recently active peer so a new agent can make its first DHT
connection.

## Why Partition the Peer Network?

A single cluster-wide peer network gives the highest opportunity for sharing:
one cached object can serve any node in the cluster. It should remain the
default until measurements show a reason to divide it.

Partitioning provides a tool for clusters that need a smaller or more local
peer network. Possible reasons include:

- bounding the size and churn of each distributed hash table (DHT);
- keeping peer transfers within an availability zone or network locality;
- separating hardware or workload pools operationally;
- containing peer-network failures to one part of the cluster; and
- allowing different groups to evolve independently in the future.

Partitioning has a direct cost: content is no longer shared across group
boundaries. If the same new image layer is requested in ten groups, each group
may fetch and cache its own copy. The design therefore treats partitioning as
an explicit operational choice, not an automatic optimization.

## What a Peer Group Means

A peer group is a hard discovery and coordination boundary.

After partitioning, each group has its own:

- bootstrap Lease slots;
- DHT routing network;
- content-provider records;
- cold-puller coordination; and
- remembered bootstrap peers.

An agent first checks its node-local containerd content store. On a local miss,
it searches for providers inside its group. If no provider exists, it chooses
a cold puller from that group. The selected peer fetches the object from the
origin registry and advertises it to the group.

```text
Node label: topology.kubernetes.io/zone

  eastus-1                    eastus-2
  +------------------+        +------------------+
  | Gantry group A   |        | Gantry group B   |
  |                  |        |                  |
  |  node 1 <-> 2    |        |  node 4 <-> 5    |
  |      \     /      |        |      \     /      |
  |       node 3     |        |       node 6     |
  +------------------+        +------------------+
       own DHT                    own DHT
       own providers              own providers
       own cold pulls             own cold pulls
```

There is no cross-group lookup after partitioning. A cross-group fallback would
reconnect the routing and failure domains that partitioning is intended to
separate.

A peer group is not a security or tenancy boundary. Separate discovery
networks prevent accidental cross-group routing, but they do not prove that a
peer is entitled to join a group. Enforcing group membership as authorization
would require an additional identity and attestation design.

## Goals

- Derive Gantry peer groups from a configurable Kubernetes Node label.
- Support labels chosen by the operator rather than a fixed notion of zone.
- Partition an already-running cluster with one Gantry DaemonSet rollout.
- Keep image pulls available while old and new peer networks coexist.
- Avoid Pod or Node list/watch operations in each Gantry agent.
- Preserve node-local cached content across Gantry Pod replacement.
- Support later changes from one grouping policy to another.

## Non-goals

- Automatically deciding whether a cluster should be partitioned.
- Automatically balancing group sizes.
- Sharing provider records or coordinating cold pulls across final groups.
- Treating a Node label as a security credential.
- Reacting immediately to a label change on a running Node.
- Guaranteeing one origin pull across the entire Kubernetes cluster after it
  has been partitioned.
- Preserving cluster-wide peer discovery or cold-pull coordination during the
  rollout.

## Assigning a Node to a Group

The operator configures a partition identifier and a Node label key. An
illustrative configuration is:

```yaml
peer_partition:
  id: zone-v1
  node_label: topology.kubernetes.io/zone
  missing_label: reject
```

The partition identifier names one version of the grouping policy. It prevents
agents using different label keys or assignments from accidentally joining the
same peer network. For example, a later rack-based layout uses a new identifier
even if one of its label values happens to match a zone name.

When an agent starts, it:

1. learns its Kubernetes Node name from its Pod environment;
2. reads that one Node from the Kubernetes API;
3. reads the configured label value; and
4. derives its peer-group identity from the partition identifier and label
  value.

The label value remains visible to operators, while Gantry may derive a
fixed-length internal identifier for protocol and Kubernetes object names.
This avoids placing arbitrary label text directly into names with stricter
length or character limits.

This costs one Node read per Gantry Pod start. A rollout across `N` nodes
therefore performs approximately `N` Node reads. Agents do not list or watch
Nodes, so this does not recreate complete membership or `N^2` object delivery.

Standard Kubernetes authorization cannot dynamically grant a Pod permission
to read only the Node named in its own `spec.nodeName`. A normal `get` grant on
Nodes can therefore read other Nodes as well, even though Gantry will request
only its own. This permission tradeoff must be accepted or replaced by a
separate component that copies the selected Node label onto each Gantry Pod.

### Missing labels

Missing labels must have an explicit policy:

- **Reject:** the node has no resolved group and cannot complete partition
  rollout. This is the safer default.
- **Fallback group:** unlabeled nodes join a named group such as `default`.
  This is useful when unlabeled nodes are intentional, but may create one large
  residual group.

The rollout should inspect label coverage and group sizes before replacing any
agents. That is a one-time operator action; it is not steady-state membership
maintained by every Gantry Pod.

### Label changes

A running agent does not watch its Node. Changing a label does not move that
agent immediately. The new assignment takes effect when its Gantry Pod is
restarted, normally through another DaemonSet rollout.

This behavior is deliberate. Silently moving one peer between DHTs in response
to an arbitrary label edit would bypass normal rollout controls.

## Preparing the Groups

Before the rollout, the operator:

1. chooses the Node label and partition identifier;
2. verifies that every eligible Node has an accepted label value;
3. reviews the number and size of the resulting groups; and
4. creates a fixed set of bootstrap Lease slots for every resulting group.

The Lease object count is proportional to the number of distinct groups, not
the number of nodes. If there are `G` label values and `S` slots per group,
Kubernetes stores:

```text
G * S Lease objects
```

The Lease slots are created before the rollout so agents need only read and
update known objects. A new label value introduced later requires its Lease
slots to be created before nodes move into that group.

## Live Rollout

The existing agents begin in one cluster-wide DHT. The replacement DaemonSet
configuration enables label-based partitioning.

For each Pod replaced by the rolling update, the new agent:

1. reads its local Node and resolves the configured label;
2. selects the group identified by the partition ID and label value;
3. reads or claims bootstrap Lease slots for that group;
4. joins that group's DHT; and
5. advertises content already present in the node's containerd store.

It does not join the old cluster-wide DHT.

```text
rollout begins

old agents  -> cluster-wide DHT
new agents  -> label-derived DHTs

rollout completes

all agents  -> label-derived DHTs
```

The first new agent for a group finds no group peer. It claims an available
Lease slot and starts a one-node DHT. Later agents in that group discover it
through the group's Lease slots.

During the rollout, old and new agents do not exchange provider records or
coordinate cold pulls. Content cached only on an old agent is temporarily
invisible to new agents. A new group may therefore fetch that content from the
origin even though another copy exists elsewhere in the cluster.

This is expected migration behavior. Gantry continues serving image pulls
because each population retains its normal origin fallback. When an updated
agent starts, it immediately makes that node's existing containerd content
discoverable inside its new group.

No group-aware rollout controller is required. The DaemonSet's ordinary
availability limit controls how many node-local Gantry endpoints are replaced
at once. If a small group temporarily has no running updated agent, it simply
has no peer sharing until its first member starts and forms the group.

A one-node group is valid but has no peer-sharing benefit. Its agent fetches
cold content from the origin. Operators should treat unexpectedly tiny groups
as a labeling issue unless they are intentional.

An updated agent is ready when it has resolved its group and has either joined
a group peer or successfully established itself as the first member through a
Lease slot.

## Rollback

Rollback is another ordinary DaemonSet rollout. Reverted agents leave their
label-derived groups and rejoin the cluster-wide DHT as their Pods are
replaced.

The same temporary separation applies in reverse: reverted and partitioned
agents do not discover one another, and duplicate origin pulls are possible.
Group Lease slots should remain until the rollback has completed.

## Changing the Grouping Later

The same simple rollout can move from one partitioned layout to another. For
example, an operator can move from zone groups to rack groups by choosing a new
partition identifier and label key, preparing the new Lease slots, and rolling
the DaemonSet.

Old and new layouts remain separate during that rollout. Existing node-local
content is advertised into the new layout as each Pod restarts. Changing a
single Node label follows the same rule: restart that Gantry Pod to apply the
new assignment.

## Failure Behavior

| Situation | Expected behavior |
| --- | --- |
| An updated agent cannot read its Node object | It cannot determine its group and remains unready. Existing running agents are unaffected. |
| The configured label is missing | The agent remains unready unless a fallback group is configured. |
| Group Lease slots do not exist | The agent cannot bootstrap its group and remains unready. |
| A group has one member | The agent operates as a one-node DHT and pulls cold content from origin. |
| A group loses all agents temporarily | The first returning agent reclaims a Lease slot and forms a one-node DHT; warm provider records rebuild as agents advertise local content. |
| A label changes while Gantry is running | Nothing moves until the managed restart or rollout. |
| Old and new agents request the same cold digest during rollout | Each peer network may perform its own origin pull. |
| The rollout is reversed | Reverted agents progressively rebuild the cluster-wide DHT. |

## Scale and Cost

Let:

- `N` be the number of Gantry nodes;
- `G` be the number of distinct accepted label values;
- `S` be the fixed number of Lease slots per group; and
- `N_g` be the number of nodes in one group.

Then:

- a full Gantry rollout performs approximately `N` local Node reads;
- Kubernetes stores `G * S` group rendezvous Leases;
- each DHT contains approximately `N_g` agents;
- label skew directly creates DHT size skew; and
- the same cold digest may produce approximately one origin pull per group
  under normal operation.

These are structural relationships, not measured performance results. The DHT
resource reduction, transfer locality, duplicate origin traffic, and rollout
duration must be measured for the actual label distribution.

Labels with very high cardinality, such as a unique hostname, can accidentally
create one peer group per node. Preflight must report distinct values, group
sizes, and singleton groups before a rollout begins.

## Operations and Observability

Operators need to see:

- the configured partition identifier and label key;
- each updated agent's resolved group;
- missing or rejected labels;
- group bootstrap and readiness;
- Lease claims and stale contacts by group;
- provider and cold-puller lookup outcomes by group;
- duplicate origin pulls during and after rollout; and
- rollout progress for old and updated agents.

Raw arbitrary label values should not be copied onto every metric without a
cardinality review. Logs and a bounded group-information metric can expose the
operator-facing value while high-volume metrics use a stable internal group
identifier.

## Validation

Before label-based partitioning is supported, tests must establish that:

- a cluster-wide DHT can be split while image pulls continue;
- each group can form from an initially empty DHT;
- updated agents advertise existing containerd content into their group;
- old and updated agents remain isolated during rollout;
- no cross-group provider is returned after rollout;
- missing labels and missing Lease slots stop migration safely;
- single-node and highly skewed groups recover after restart;
- duplicate origin pulls during transition remain within observed limits;
- rollback progressively restores the cluster-wide DHT; and
- a later label change can move nodes between groups through another rollout.

## Open Decisions

1. Should a missing label reject rollout by default, or should `default` be the
   default fallback group?
2. What minimum group size should produce a warning or block rollout?
3. Should group membership remain an operational boundary, or eventually be
   enforced as an authorization boundary?
4. How should the operator enumerate groups and render their fixed Lease
   slots in the deployment workflow?

## Proposed Direction

The proposed model is:

- one configurable Node label determines peer-group assignment;
- one startup read of the local Node replaces any need for a Node informer;
- each partition identifier and label value identifies an independent Gantry
  peer group;
- one ordinary DaemonSet rollout moves agents directly from the cluster-wide
  DHT into their label-derived groups;
- temporary loss of cross-layout peer discovery and duplicate origin pulls are
  accepted during rollout; and
- subsequent label or policy changes use the same simple rollout model.

Partitioning remains optional. Clusters that do not need it continue using one
cluster-wide `default` group.