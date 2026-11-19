# Node Agent Version Reporting

The daemon controller publishes the running agent's product name and version in
Machina `Machine.status.agent.version`. Unbounded and other consumers supply
their own build information when setting up the shared controller.

This proposal adds optional, best-effort reporting to `pkg/agent/daemon`. A
missing Machine CRD or Machine object skips publication without preventing the
shared controller from starting.

## Goals

- Expose the running agent's product name and version on its Machine.
- Share the publisher between Unbounded and other consumers.
- Publish after daemon startup and repair missing or changed reports.
- Keep reporting independent of host-local lifecycle operations.

## Non-goals

- Create Machine objects or install Machina CRDs.
- Change the consumer's existing Machine registration requirements or agent
  upgrade-completion flow.

## Machine Status

`AgentStatus` currently contains only the applied rootfs image
(`api/machina/v1alpha3/machine_types.go:1222`). Add an optional `Version string`
field, serialized as `version`:

```yaml
status:
  agent:
    image: ghcr.io/azure/rootfs:v1.0.0
    version: unbounded-agent/v0.0.10
```

The value has the format `<agent-name>/<version>`. An unstamped Unbounded build
reports `unbounded-agent/dev`. Consumers split on the first `/` and treat the
remainder as an opaque version.

The field records the product and version most recently reported by the running
daemon. An absent field means the version has not been reported. A stopped
daemon leaves its last report behind, so the value is not a heartbeat, readiness
signal, or upgrade-success condition.

Use the executing product's build metadata rather than desired installation
settings or the Unbounded library version embedded in another product. A download
URL, image tag, or executable symlink does not establish which binary is running.

## Controller Setup

Extend `SetupController` in `pkg/agent/daemon/setup.go:22` with an options argument:

```go
type AgentInfo struct {
    Name    string
    Version string
}

type ControllerOptions struct {
    MachineName string
    AgentInfo   *AgentInfo
}

func SetupController(
    name string,
    mgr ctrl.Manager,
    machineOperations MachineOperationRequestReconciler,
    repaves RepaveReconciler,
    options ControllerOptions,
) error
```

A nil `AgentInfo` disables reporting. When enabled, the caller supplies:

- `MachineName`: the nonempty name of the target Machina Machine, independent of
  the Node name or any platform resource identifier.
- `AgentInfo.Name`: a nonempty product name without `/`.
- `AgentInfo.Version`: the nonempty version of the executing product.

The publisher snapshots these values during setup. The product identity and
version come from executable build metadata, not persisted provisioning
settings. The shared controller owns publication to Machine status.

### Product Integration

| Caller | `AgentInfo.Name` | `AgentInfo.Version` |
|---|---|---|
| Unbounded | `unbounded-agent` | `internal/version.Version` |
| Other consumer | Its own product name | Its own executable's build version |

Unbounded passes its Machine name and build version from
`cmd/agent/internal/daemon/controller.go`. The version defaults to `dev` for
unstamped builds (`internal/version/version.go:22`).

Other consumers pass their own product name, build version, and the Machina
Machine name associated with their worker. Publication waits for that Machine
to exist.

## Publication Flow

When reporting is enabled, `SetupController` registers a manager runnable that
attempts publication at startup and then once per minute. Each attempt:

1. Reads the configured Machine through the manager's uncached API reader.
2. Skips publication if the Machine API mapping is absent or the Machine is not
   found, without logging an error.
3. Formats the configured identity as `<agent-name>/<version>`.
4. Skips the write if `status.agent.version` already matches.
5. Otherwise, patches that field through the status subresource.

Use a JSON merge patch containing only the daemon-owned field:

```json
{"status":{"agent":{"version":"unbounded-agent/v0.0.10"}}}
```

The patch creates `status.agent` when absent and preserves the applied image,
other status fields, and spec. It carries no resource-version precondition
because the daemon is the only writer of `version`. Other status writers retain
their existing optimistic concurrency behavior.

Periodic publication handles Machines created or recreated after startup and
repairs removed or changed version reports. A daemon restarted after an upgrade
or rollback publishes its own configured version. The process staging a
candidate does not publish on that candidate's behalf.

### Manager Integration

The reporting runnable runs outside the serialized lifecycle queue. It does not
acquire host mutation locks, and publication failures do not block repave, reset,
restart, or upgrade. Manager cancellation stops reporting without clearing the
last published value.

Register the Machine Go type in the client's scheme and use the manager's API
reader and write client. Reporting does not register a Machine informer, so a
missing CRD cannot block its startup. Missing API discovery is retried on later
attempts rather than cached for the process's lifetime.

### Publication Failures

An absent API or Machine is an expected skip. Other API errors, including
Forbidden, are logged and retried on the next interval. A Forbidden response is
not treated as absence and does not trigger a credential or authorization
change. These failures never stop the manager.

## Status Ownership

| Field | Writer |
|---|---|
| `status.agent.version` | The running product daemon |
| `status.agent.image` | Machina's provisioning controller |

Machina currently replaces the entire agent status when recording the applied
image (`cmd/machina/machina/controller/machine_controller.go:454`). Change that
writer to initialize `status.agent` only when nil and otherwise update only
`image`. This preserves a version reported before provisioning completes. Keep
the existing applied-image assertions
(`cmd/machina/machina/controller/machine_controller_test.go:343`).

A Machine has one product daemon reporting its version. Unbounded and other
consumers are alternative writers, not concurrent publishers. The report is
self-reported inventory, not attestation.

## RBAC

The publisher uses the manager's existing credentials. Unbounded already grants
Machine reads and `machines/status` patch/update access to
`unbounded-agent-daemons`
(`deploy/machina/07-bootstrapper-rbac.yaml.tmpl:14`,
`deploy/machina/07-bootstrapper-rbac.yaml.tmpl:37`). No RBAC change is needed for
that daemon. Other consumers supply a client authorized for the same operations.
The existing status grant is group-wide, not a per-Machine authorization boundary.

## Availability and Rollout

The reporter must tolerate missing Machina APIs independently of the consumer's
other controllers. This does not remove existing Machina dependencies elsewhere
in an application. In particular, Unbounded currently requires successful
Machine registration before calling the shared controller setup
(`cmd/agent/internal/daemon/daemon.go:181`). Changing that startup requirement is
separate from this publisher.

Deploy the generated CRD schema containing `status.agent.version` before enabling
publication against an installed Machine API. Regenerate the CRD and deepcopy
outputs through `make generate`; do not edit generated files by hand.

An operator can inspect the resulting field with:

```sh
kubectl get machines -o custom-columns=NAME:.metadata.name,AGENT:.status.agent.version
```
