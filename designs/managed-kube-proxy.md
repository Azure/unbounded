# Managed kube-proxy for Unbounded Sites

## Problem

Unbounded worker nodes can join a Kubernetes cluster outside the cloud
provider's managed node pools. On AKS, the provider-owned `kube-proxy`
DaemonSet selects AKS nodes with provider labels such as
`kubernetes.azure.com/cluster`. Externally joined unbounded nodes do not match
that selector, so no kube-proxy process programs ClusterIP service rules on
those hosts.

The unbounded-net node agent can still route to real pod and host endpoints, but
ClusterIP addresses do not work. This breaks direct status push to the
unbounded-net controller service because traffic to the service IP is never
DNATed to the controller endpoint.

## Goals

- Run kube-proxy on unbounded-managed site nodes that are not covered by the
  cluster provider's kube-proxy DaemonSet.
- Avoid running two kube-proxy instances on the same node.
- Preserve provider-owned kube-proxy DaemonSets, especially managed AKS
  resources that may be reconciled by addon managers.
- Keep kube-proxy configuration site-aware so local traffic detection uses the
  site's pod CIDR.

## Non-goals

- Replacing provider-owned kube-proxy on managed cluster nodes.
- Supporting one kube-proxy process with multiple unrelated IPv4 pod CIDRs.
  Kubernetes kube-proxy validates `--cluster-cidr` as either a single CIDR or a
  dual-stack pair, so multiple IPv4 site CIDRs require separate DaemonSets.

## Behavior

The unbounded-net controller manages one kube-proxy DaemonSet per Site:

```text
unbounded-net-kube-proxy-<site>
```

Each DaemonSet is scheduled only to nodes with both labels:

```text
net.unbounded-cloud.io/site=<site>
net.unbounded-cloud.io/kube-proxy=managed
```

The controller adds `net.unbounded-cloud.io/kube-proxy=managed` to nodes when:

- the node has a canonical unbounded site label, and
- the node is not an AKS/provider-managed node. Currently this excludes nodes
  with `kubernetes.azure.com/cluster` or `kubernetes.azure.com/managedby`, and
- no provider-owned kube-proxy DaemonSet appears to cover the node.

The controller removes the marker when those conditions stop being true.

Provider kube-proxy DaemonSets are detected by kube-proxy container name/image
and excluded if they are unbounded-owned. The controller evaluates their node
selectors and required node affinity against each node. Nodes already matched by
provider kube-proxy are not labeled for unbounded-managed kube-proxy.

The controller owns only DaemonSets labeled
`app.kubernetes.io/name=unbounded-net-kube-proxy`. It does not modify provider
DaemonSets such as AKS `kube-system/kube-proxy`.

Sites without an enabled pod CIDR assignment are skipped because kube-proxy
requires a valid `--cluster-cidr` for `ClusterCIDR` local traffic detection.
Unbounded-owned DaemonSets whose site no longer exists are deleted.

## DaemonSet Template

The managed DaemonSet uses the cluster's existing `kube-system/kube-proxy` image
when available. If that DaemonSet does not exist, it falls back to
`registry.k8s.io/kube-proxy:<server version>`.

Image selection can be overridden with:

```yaml
controller:
  managedKubeProxy:
    image: <image>
```

Managed kube-proxy can be disabled with:

```yaml
controller:
  managedKubeProxy:
    enabled: false
```

The equivalent controller flags are:

```text
--managed-kube-proxy=false
--managed-kube-proxy-image=<image>
```

The pod runs with:

- `hostNetwork: true`
- `system-node-critical` priority
- privileged security context
- broad tolerations
- `/run/xtables.lock`, `/etc/sysctl.d`, and `/lib/modules` host mounts
- `kubernetes.azure.com/set-kube-service-host-fqdn: "true"`

The AKS service host FQDN annotation is important for bootstrapping. Before
kube-proxy programs service rules, `KUBERNETES_SERVICE_HOST=10.0.0.1` may be
unreachable from the node. The FQDN override lets kube-proxy contact the API
server without relying on ClusterIP service NAT.

An init container runs before kube-proxy to set `nf_conntrack_max` using the
same CPU-scaled floor used by AKS kube-proxy. This preserves the provider's
expected conntrack sizing on externally joined nodes that do not run the
provider-owned DaemonSet.

The kube-proxy command uses the first enabled IPv4 pod CIDR for the Site, plus
the first enabled IPv6 pod CIDR if present:

```text
--cluster-cidr=<site IPv4>[,<site IPv6>]
--detect-local-mode=ClusterCIDR
```

For the `test` site this is:

```text
--cluster-cidr=100.125.0.0/16
```

## RBAC

The unbounded-net controller needs cluster-wide DaemonSet read/write access to
detect provider kube-proxy and manage unbounded-owned DaemonSets.

Managed kube-proxy pods use a dedicated service account:

```text
unbounded-net/unbounded-net-kube-proxy
```

That service account is bound to the built-in `system:node-proxier` ClusterRole.

The controller service account also needs cluster-wide DaemonSet permissions so
it can detect provider kube-proxy DaemonSets and create/update/delete the
unbounded-owned per-site DaemonSets.

## Operational Notes

- Deleting a Site deletes or stops updating its managed kube-proxy DaemonSet.
- Changing a Site's pod CIDR assignment updates the corresponding DaemonSet and
  rolls kube-proxy for that site.
- If a provider later starts covering an unbounded node, the controller removes
  the managed marker label, and the unbounded-owned kube-proxy pod drains from
  that node.
- A single DaemonSet for all sites was tested but rejected because kube-proxy
  does not accept multiple IPv4 `--cluster-cidr` values.
