# Agent Preflight

`unbounded-agent preflight` validates that a host and agent configuration are
ready for node bootstrap before the agent mutates host state or joins the
cluster. The command is owned by the node agent because only the agent runs on
the target host with direct access to systemd, systemd-nspawn, kernel state,
network reachability, artifact mirrors, and GPU devices.

This design intentionally focuses on the standalone preflight command.

## Goals

- Provide an agent-native command that can run on the node before bootstrap.
- Reuse existing agent config loading and goal-state resolution behavior where
  possible.
- Detect hard bootstrap blockers before host mutation begins.
- Report warnings and fatal errors in a familiar kubeadm-style format.
- Allow selected fatal checks to be downgraded to warnings for break-glass
  scenarios.
- Support machine-readable output for automation.
- Keep the command non-mutating.

## Non-goals

- Do not add offline deployment flags to the preflight command itself.
- Do not perform package installation, rootfs provisioning, image pulls, or
  other durable host changes.
- Do not solve the full offline deployment config model in this document.

## Kubeadm reference

Kubeadm provides a useful model for this experience. Its preflight
implementation uses small check units with a shared interface:

```go
type Checker interface {
    Check() (warnings, errorList []error)
    Name() string
}
```

Kubeadm runs these checks before cluster initialization or join work, prints
warnings and errors separately, fails on errors by default, and lets users make
specific checks non-fatal with `--ignore-preflight-errors`:

```bash
kubeadm init phase preflight --config kubeadm-config.yaml
kubeadm init --ignore-preflight-errors=Swap,SystemVerification
kubeadm init --ignore-preflight-errors=all
```

The user-facing output is simple and recognizable:

```text
[preflight] Running pre-flight checks
    [WARNING Swap]: swap is enabled
    [ERROR IsPrivilegedUser]: user is not running as root
[preflight] Some fatal errors occurred:
...
[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`
```

`unbounded-agent preflight` should follow the same semantics where they fit:

- Checks have stable names.
- Checks return warnings and errors separately.
- Errors are fatal by default.
- Ignored errors are downgraded to warnings.
- `all` ignores all preflight errors.
- Text output is optimized for humans.
- Structured output is available for automation.

## Command UX

The primary command is:

```bash
unbounded-agent preflight
```

The command should load configuration through the existing agent config loading
behavior.

Selected checks can be made non-fatal:

```bash
unbounded-agent preflight --ignore-preflight-errors=swap-active
unbounded-agent preflight --ignore-preflight-errors=nvidia-runtime,nvidia-driver-libraries
unbounded-agent preflight --ignore-preflight-errors=all
```

Warnings do not fail the command by default. Operators can choose to fail on any
warning:

```bash
unbounded-agent preflight --fail-on-warnings
```

Automation can request JSON output:

```bash
unbounded-agent preflight --output json > preflight-report.json
```

Initial flags:

| Flag | Meaning |
|---|---|
| `--ignore-preflight-errors` | Comma-separated check names whose errors should be reported as warnings. The special value `all` ignores all errors. |
| `--fail-on-warnings` | Exit non-zero when any warning is returned, even if there are no fatal errors. |
| `--output` | Output format. Supported values: `text`, `json`. Default: `text`. |

The command should not accept deployment-shaping flags such as `--offline`,
`--gpu`, or mirror URLs. Preflight validates the loaded agent config; it does
not define the deployment config.

## Command semantics

`unbounded-agent preflight` should:

1. Initialize command logging.
2. Load agent config.
3. Apply existing config normalization.
4. Resolve machine goal state far enough to know expected rootfs, downloads,
   kubelet settings, and host integrations.
5. Build the applicable check list from the config and resolved goal state.
6. Run checks.
7. Print warnings and errors.
8. Exit `0` when no fatal errors remain after ignore rules and
   `--fail-on-warnings` is not set.
9. Exit non-zero when one or more fatal errors remain, or when warnings are
   present and `--fail-on-warnings` is set.
10. Avoid durable host mutation.

The command should be safe to run repeatedly before bootstrap and after failed
bootstrap attempts.

## Check model

The preflight framework should use a result-oriented checker interface so it can
report successful checks, warnings, ignored errors, and fatal errors through the
same model:

```go
type Checker interface {
    Name() string
    Check(ctx context.Context) []Result
}

type Result struct {
    Name     string
    Target   string
    Severity Severity // ok, warning, error
    Message  string
    Ignored  bool
}
```

The runner is responsible for applying `--ignore-preflight-errors`, formatting
results, and returning a fatal error when required. An ignored error is reported
as a warning with `Ignored: true`:

```text
[WARNING swap-active]: swap is enabled
```

If `--fail-on-warnings` is set, any warning causes the command to exit non-zero,
including warnings created by ignored errors.

Severity should follow a simple policy:

- Return a warning when bootstrap can safely remediate the condition without
  external input. For example, active swap can be a warning when bootstrap will
  disable it.
- Return a warning when bootstrap can remediate the condition by installing or
  reconfiguring host state. For example, missing required host packages can be a
  warning when package source access is allowed.
- Return an error when bootstrap cannot proceed or remediation requires external
  input. For example, missing required host packages should become an error in
  offline mode.
- Return an error when continuing would risk joining the node with incorrect
  identity, credentials, rootfs, runtime, or GPU behavior.

### Package ownership

The reusable preflight framework should live in:

```text
pkg/agent/preflight
```

This package owns the common report model, severity handling,
`--ignore-preflight-errors` behavior, `--fail-on-warnings` behavior, and runner
logic:

```go
func Run(ctx context.Context, checks []Checker, opts Options) Report
```

The check implementation should be reusable outside the agent command so
external callers can run the same checks and consume the same report model. The
command package should only handle CLI flags, config loading, invoking the
preflight package, and formatting command output.

Concrete checker constructors should live near the phase they validate. The
phase package owns the bootstrap behavior, so it should also own the
non-mutating checks that predict whether that behavior will succeed.

Examples:

```text
pkg/agent/phases/host
  InstallPackages(...)
  CheckHostPackages(...)
  CheckHostOSConfiguration(...)

pkg/agent/phases/rootfs
  Provision(...)
  CheckRootFSProvisioning(...)
  CheckKubernetesArtifacts(...)
  CheckCRIArtifacts(...)
  CheckCNIArtifacts(...)

pkg/agent/phases/nodestart
  StartNode(...)
  CheckNSpawnRuntime(...)
```

The agent command composes these phase-owned checkers after loading config and
resolving goal state:

```go
checks := []preflight.Checker{
    preflight.AgentConfig(cfg),
    host.CheckHostPackages(log),
    host.CheckHostOSConfiguration(log),
    rootfs.CheckOCIImageReachable(rootFSGoalState),
    rootfs.CheckKubernetesArtifacts(rootFSGoalState),
    rootfs.CheckCRIArtifacts(rootFSGoalState),
    rootfs.CheckCNIArtifacts(rootFSGoalState),
    nodestart.CheckNSpawnRuntime(nodeStartGoalState),
}

report := preflight.Run(ctx, checks, opts)
```

Shared implementation should be factored below both the mutating task and the
checker. For example, artifact preflight should use the same URL resolution,
download, decompression, and verification helpers used by rootfs provisioning,
but it should not call the mutating rootfs task itself.

Checks may use temporary files or temporary directories when they need to reuse
the same download, decompression, verification, or registry code paths as
bootstrap. Temporary state must be cleaned up before the check returns and must
not change durable host state.

### Phase-aligned organization

Preflight checks should be grouped around the same conceptual phases and task
names used by bootstrap. This keeps the output actionable because a failed
check points at the bootstrap step that would fail later.

For example:

```text
[WARNING host-packages]: required host packages are missing and may be installed by bootstrap: systemd-container, curl
[WARNING swap-active]: swap is active and bootstrap will disable it
[ERROR oci-image-reachable]: failed to resolve rootfs image manifest (target: rootfs image)
```

The check name should remain stable and ignoreable, but it does not need to be
derived from a task name. Each checker can return an ad-hoc name that best
describes the condition it validates. Check names should use kebab-case so they
are easy to read in CLI output and pass to `--ignore-preflight-errors`.
Phase/task grouping can be represented in code structure, comments, or optional
metadata for JSON output without becoming part of the check name.

Examples:

```text
agent-config
host-packages
swap-active
oci-image-reachable
kubernetes-artifacts
cri-artifacts
cni-artifacts
nvidia-driver-libraries
```

The ignore flag should accept stable check names:

```bash
unbounded-agent preflight --ignore-preflight-errors=swap-active
unbounded-agent preflight --ignore-preflight-errors=nvidia-driver-libraries
```

The minimum requirement is exact check-name matching plus `all`, matching
kubeadm's mental model.

## Text output

Default output should be kubeadm-like:

```text
[preflight] Running unbounded-agent pre-flight checks
    [WARNING swap-active]: swap is enabled and will be disabled during bootstrap
    [WARNING host-packages]: required host packages are missing and may be installed by bootstrap: systemd-container, curl
    [ERROR oci-image-reachable]: failed to resolve rootfs image manifest (target: rootfs image)
[preflight] Some fatal errors occurred:
    [ERROR oci-image-reachable]: failed to resolve rootfs image manifest (target: rootfs image)
[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`
```

Warnings should be printed as they are discovered. Fatal errors may be buffered
and summarized at the end, matching kubeadm's behavior.

Preflight output must not print raw configured values such as URLs, image
references, tokens, certificate data, file contents, or credential-bearing
strings. Reports should include only the logical target being checked, such as
`rootfs image`, `kubernetes artifacts`, `cluster API server`, or
`bootstrap credential`.

Checkers must sanitize or wrap errors from lower-level libraries before adding
them to a report. Download, registry, TLS, file, and package-manager errors may
include raw URLs, image references, paths, or credentials; those values must not
be copied into `Result.Message`.

## JSON output

JSON output should contain every check result, including successful checks,
warnings, ignored errors, and fatal errors:

```json
{
  "status": "failed",
  "checks": [
    {
      "name": "agent-config",
      "severity": "ok",
      "message": "agent config is valid",
      "target": "agent config",
      "ignored": false
    },
    {
      "name": "host-packages",
      "severity": "warning",
      "message": "required host packages are missing and may be installed by bootstrap: systemd-container, curl",
      "target": "host packages",
      "ignored": false
    },
    {
      "name": "swap-active",
      "severity": "warning",
      "message": "swap is enabled and will be disabled during bootstrap",
      "target": "host swap",
      "ignored": false
    }
  ]
}
```

The schema should stay intentionally small. It should include `ignored` for each
check result so automation can distinguish normal warnings from ignored errors:

```json
{
  "name": "swap-active",
  "severity": "warning",
  "message": "swap is enabled and will be disabled during bootstrap",
  "target": "host swap",
  "ignored": false
}
```

Possible future fields include `category`, `suggestion`, and
`documentationURL`. The same redaction rule applies to JSON output: include
logical targets, not raw config values.

## Check set

The check set should prioritize conditions that directly predict bootstrap
failure. Checks should be outcome-oriented: report whether the host, config,
artifacts, and credentials are ready for bootstrap instead of exposing every
low-level helper command as a separate check.

Config check:

| Check | Purpose |
|---|---|
| `agent-config` | Validate the loaded agent config and return config errors for missing required fields, invalid values, inconsistent settings, unsupported Kubernetes versions, missing OCI rootfs image, invalid download source templates, or invalid kubelet auth configuration. |

Host phase checks:

| Check | Purpose |
|---|---|
| `is-privileged-user` | Ensure the command is running as root. |
| `host-packages` | Validate a supported package manager exists and required host packages are installed. Missing packages are reported by name as warnings when bootstrap can install them. They should become errors in offline mode. |
| `host-os-configuration` | Validate host OS configuration can be applied: sysctl config path writable, relevant kernel parameters acceptable or settable, and systemd unit paths writable. |
| `nspawn-runtime` | Validate the host systemd environment can manage nspawn machines using installed host capabilities. Missing tools are warnings when bootstrap can install them. They should become errors in offline mode. |
| `docker-active` | Warn if Docker is active and bootstrap will disable or avoid it. |
| `swap-active` | Warn when swap is enabled if bootstrap will disable it. |
| `disk-space` | Validate enough space exists for rootfs and component downloads. |
| `cgroups` | Validate cgroup support expected by kubelet/containerd. |
| `api-server-reachable` | Validate the configured Kubernetes API server is reachable from the host. |
| `cluster-credentials` | Validate the cluster CA data and configured bootstrap credential are present and parseable for kubelet registration. |

Rootfs provisioning checks:

| Check | Purpose |
|---|---|
| `machine-dir` | Validate the target machine directory state is compatible with bootstrap. |
| `oci-image-reference` | Validate the rootfs image reference parses. |
| `oci-image-reachable` | Validate the configured rootfs image manifest can be resolved without pulling layers. |
| `rootfs-provisioning` | Validate rootfs provisioning prerequisites are available from installed host packages and host-side nspawn config paths are writable. |
| `kubernetes-artifacts` | Validate kubelet/kubectl/kube-proxy artifacts and checksums using the same download and verification calls used by rootfs provisioning, without installing files. |
| `cri-artifacts` | Validate containerd, runc, and crictl artifacts using the same download/decompression or download calls used by rootfs provisioning, without installing files. |
| `cni-artifacts` | Validate CNI plugin artifacts using the same download/decompression calls used by rootfs provisioning, without installing files. |
| `rootfs-parent-writable` | Validate the parent directory for rootfs creation can be created or written. |

GPU checks:

| Check | Purpose |
|---|---|
| `gpu-config` | Validate GPU policy/config once a GPU config block exists. |
| `nvidia-devices` | Validate expected NVIDIA device files are present. |
| `nvidia-driver-libraries` | Validate expected NVIDIA host libraries are discoverable. |
| `nvidia-runtime` | Validate NVIDIA runtime and CDI generation prerequisites when required. |

Offline checks should be added after the preflight command when the agent has an
explicit offline policy in config. They are omitted when no offline policy is
configured:

| Check | Purpose |
|---|---|
| `offline-no-upstream-urls` | Fail if offline mode resolves any artifact to an upstream default. |
| `allowed-hosts` | Validate all resolved URLs target configured allowed hosts. |
| `mirror-reachability` | Validate configured mirrors are reachable from the host. |
