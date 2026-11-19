# centerpivot

centerpivot updates Azure Container Linux (ACL) nodes from Nebraska. Its design
is [`designs/centerpivot.md`](../../designs/centerpivot.md). This file covers how
the code is laid out, how to build and test it, and the rules for changing it.

## The boundary

`cmd/centerpivot` keeps its code to itself. Only two packages are exported,
because other binaries run them:

| Package | Exports | Imported by |
|---|---|---|
| `operatorcomponent` | `New`: the operator component that installs centerpivot | `internal/operator`, for the operator's registry |
| `hostagent` | `ConfigureTridentACLAgent`, `RemoveTridentACLAgentConfig`, `HostMounts` | `pkg/agent/phases/nodestart`, `pkg/agent/phases/rootfs`, and `cmd/agent/internal/daemon` for reset |

Everything else is under `internal/`, which Go lets only `cmd/centerpivot`
import. When code outside needs something new, add it to one of these two
packages, as narrowly as you can, rather than exporting another package.

`hostagent` is linked into the agent binary, so keep it small. It imports only
the agent's own packages, `internal/names`, two small helpers from the repo's
`internal/`, and client-go's kubeconfig types.

## Layout

| Path | What |
|---|---|
| `main.go` | The binary. |
| `internal/cmd` | The command line: `controller`, `node-info`, and `version`. |
| `internal/controller` | The scheduler that hands out update slots, and the node reconciler, which takes one step for one node per pass. |
| `internal/drain` | `kubectl drain`'s rules, a step at a time, and whether the workloads moved off a node have recovered. |
| `internal/omaha` | Nebraska's update check, and the failure report that ends a grant, both shaped like trident-acl-agent's own. |
| `internal/identity` | The agent's Nebraska instance ID, os-release parsing, and the `agent-info` annotation. |
| `internal/nodeinfo` | The node-info DaemonSet, which publishes `agent-info` until Trident does. |
| `internal/contract` | trident-acl-agent's annotation contract: requests and statuses. |
| `internal/names` | The labels, taints, annotations, and paths the parts agree on. |
| `operatorcomponent` | The operator component, and the tests of the admission policies it applies. |
| `hostagent` | The host agent's part: the machine-ID bind, and trident-acl-agent's configuration. |

Outside `cmd/centerpivot` are the parts that aren't Go code, or that aren't
centerpivot's alone:

1. `deploy/centerpivot`: the manifests and admission policies the operator
   component applies.
2. `images/centerpivot`: the image.
3. `api/machina/v1alpha3/site_types.go`: the Site's `centerPivot` field
   (`CenterPivotComponentSpec`), and the CRD generated from it.
4. Lists that name centerpivot: the operator's registry
   (`internal/operator/reconciler.go`), the override allowlist
   (`internal/operator/override/validate.go`), the release and nightly image
   lists and their coverage test, the release BOM, the Makefile, and
   `.goreleaser.yml`.

## Build and test

```bash
make centerpivot-manifests   # renders deploy/centerpivot; operatorcomponent's tests need it
go test ./cmd/centerpivot/...
make centerpivot-build       # bin/centerpivot
make image-centerpivot-local
```

`make test`, `make lint`, and `make generate` cover centerpivot with the rest of
the repository.

The end-to-end test runs centerpivot on real ACL nodes, with a real Nebraska,
`trident-acl-agent`, and Trident. It needs KVM, root Podman, and ACL images; its
[README](../../hack/acl-abupdate/README.md#centerpivots-end-to-end-test) says
how to set them up:

```bash
hack/acl-abupdate/centerpivot_e2e.py test
```

Run it after changing anything a node takes part in: the host agent's part,
the request and status contract, node-info, the drain, or the manifests and
admission policies. Its scenarios cover each update path on real nodes; cases
that need many nodes or apps belong in the controller's unit tests.

## Rules

1. The reconciler never blocks. Each pass takes one step for one node and asks
   to be called again; long waits are deadlines kept in the node's state.
2. A node's state lives in its Machine's `centerpivot.unbounded-cloud.io/state`
   annotation, written with the Machine's `resourceVersion`, so two leaders
   can't both act.
3. Requests carry exactly the contract's fields, because trident-acl-agent
   refuses unknown ones. They're written as compact JSON, and the admission
   policy matches `"server":"..."` and `"appId":"..."` in that text, so change
   how requests are encoded only together with the policy.
4. Every update that stops without finishing ends its Nebraska grant: `idle()`
   and `held()` hand the update's grant to `EndGrant`, and the next pass
   reports it failed. New ways for an update to stop go through them.
5. Nebraska and the drain are behind the `Nebraska` and `Drainer` interfaces, so
   tests use fakes. The controller's tests have a harness that plays the node's
   agent.
6. Tests cover the core behavior, table-driven, with `t.Parallel()`.
