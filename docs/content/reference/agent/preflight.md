---
title: "Preflight"
weight: 2
description: "Validate a host and agent configuration before unbounded-agent bootstrap."
---

`unbounded-agent preflight` validates that a host and the loaded agent
configuration are ready for bootstrap before the agent mutates host state or
joins the cluster. It loads the same config used by `unbounded-agent start`,
resolves the expected nspawn rootfs and download inputs, and runs non-mutating
checks against the host, cluster API server, artifact sources, and GPU driver
state.

Preflight is safe to run repeatedly before bootstrap and after failed bootstrap
attempts. It is expected to run as a privileged user on the host because several
checks need direct access to host systemd, kernel, filesystem, and device state.

## Usage

Run preflight on the target host:

```bash
unbounded-agent preflight --config /etc/unbounded/agent.json
```

If `--config` is omitted, the agent uses the same config loading behavior as
`unbounded-agent start`, including the `UNBOUNDED_AGENT_CONFIG_FILE`
environment variable.

```bash
UNBOUNDED_AGENT_CONFIG_FILE=/etc/unbounded/agent.json \
  unbounded-agent preflight
```

Preflight does not install packages, pull the rootfs image, download full
binary artifacts, create the nspawn machine, start kubelet, or join the
cluster.

Generated install and bootstrap scripts run preflight by default before
`unbounded-agent start`. Set `AGENT_PREFLIGHT=false` in the target host
environment to skip that automatic preflight step.

## Flags

| Flag | Description |
|---|---|
| `--config` | Path to the agent config file. When omitted, the agent uses its normal config loading behavior. |
| `--ignore-preflight-errors` | Comma-separated check names whose errors should be reported as warnings. Use `all` to ignore all fatal checks. |
| `--fail-on-warnings` | Exit non-zero when any warning is returned, even if there are no fatal errors. |
| `--output` | Output format. Supported values are `text` and `json`. Default: `text`. |

The generated install script also honors `AGENT_PREFLIGHT`. It defaults to
running preflight. Set `AGENT_PREFLIGHT=0`, `AGENT_PREFLIGHT=false`, or
`AGENT_PREFLIGHT=no` to skip preflight before `unbounded-agent start`.

Examples:

```bash
unbounded-agent preflight --config /etc/unbounded/agent.json
unbounded-agent preflight --ignore-preflight-errors=swap-active
unbounded-agent preflight --ignore-preflight-errors=nvidia-driver
unbounded-agent preflight --ignore-preflight-errors=all
unbounded-agent preflight --fail-on-warnings
unbounded-agent preflight --output json > preflight-report.json
```

## Exit Behavior

Warnings do not fail the command by default. Errors are fatal unless they are
ignored with `--ignore-preflight-errors`.

| Result | Exit code |
|---|---|
| No fatal errors and `--fail-on-warnings` is not set | `0` |
| One or more fatal errors remain | Non-zero |
| One or more warnings exist and `--fail-on-warnings` is set | Non-zero |

Ignored errors are downgraded to warnings in the report. If
`--fail-on-warnings` is set, ignored errors still cause a non-zero exit because
they are warnings.

## Text Output

Text output is intended for humans and follows a kubeadm-style format:

```text
[preflight] Running unbounded-agent pre-flight checks
    [OK is-privileged-user]: preflight is running as root (target: host user)
    [WARNING swap-active]: swap is enabled and bootstrap will disable it (target: host swap)
[preflight] Some fatal errors occurred:
    [ERROR oci-image-reachable]: rootfs image source is not reachable: rootfs-image (target: rootfs image)
[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`
```

Successful checks and warnings are printed as check results. Fatal errors are
summarized at the end.

## JSON Output

JSON output contains every result, including successful checks, warnings,
ignored errors, and fatal errors:

```json
{
  "status": "failed",
  "checks": [
    {
      "name": "is-privileged-user",
      "target": "host user",
      "severity": "ok",
      "message": "preflight is running as root",
      "ignored": false
    },
    {
      "name": "swap-active",
      "target": "host swap",
      "severity": "warning",
      "message": "swap is enabled and bootstrap will disable it",
      "ignored": false
    },
    {
      "name": "oci-image-reachable",
      "target": "rootfs image",
      "severity": "error",
      "message": "rootfs image source is not reachable: rootfs-image",
      "ignored": false
    }
  ],
  "summary": {
    "ok": 1,
    "warnings": 1,
    "errors": 1
  }
}
```

The `status` field is `ok` when the command would exit successfully and
`failed` when fatal errors remain or `--fail-on-warnings` makes warnings fail.

## Config Loading and Goal Resolution

Before running the named checks below, the command loads the agent config,
validates it, and resolves the machine goal state used by bootstrap. Failures
at this stage are returned as command errors before a preflight report is
printed. For example, invalid JSON, missing required config fields, invalid
bootstrap authentication, unsupported download settings, or an unresolvable
rootfs goal state can stop the command before check output is produced.

## Current Checks

After config loading and goal resolution succeed, the current preflight check
set is organized around the same bootstrap phases as `unbounded-agent start`.

### Host Checks

| Check | Severity when failing | What it validates |
|---|---|---|
| `is-privileged-user` | Error | Preflight is running as root. Bootstrap requires root privileges. |
| `host-packages` | Error or warning | A supported package manager is available (`apt-get`, `tdnf`, or `dnf`) and required host packages are installed. Missing package manager support is fatal. Missing packages are warnings because bootstrap may install them. |
| `host-os-configuration` | Error | Host OS configuration paths are writable, including the sysctl config directory and systemd unit directory. |
| `nspawn-runtime` | Warning | Required nspawn runtime tools are available: `systemctl`, `machinectl`, and `systemd-nspawn`. It also checks that `/run/systemd/system` is available. Missing tools are warnings because bootstrap may install them. |
| `docker-active` | Warning | Docker is active. Bootstrap will disable Docker when needed. |
| `swap-active` | Warning | Swap is active or swap state cannot be determined. Bootstrap will disable active swap. |
| `disk-space` | Error | At least 8 GiB is available under `/var/lib` for machine rootfs and artifacts. |
| `cgroups` | Error | The cgroup filesystem exists at `/sys/fs/cgroup`. |
| `nvidia-driver` | Error or warning | If NVIDIA GPU hardware or device nodes are detected, validates the host NVIDIA driver stack. Hosts without NVIDIA GPUs pass this check. |

The `nvidia-driver` check can return multiple results with different targets:

| Target | Severity when failing | What it validates |
|---|---|---|
| `NVIDIA PCI hardware` | OK | NVIDIA PCI devices were detected. |
| `NVIDIA PCI driver` | Error or warning | NVIDIA PCI devices are bound to the `nvidia` kernel driver. Device nodes without matching PCI hardware are reported as a warning. |
| `NVIDIA kernel modules` | Error or warning | Required modules are loaded: `nvidia`, `nvidia_modeset`, `nvidia_uvm`, and `nvidia_drm`. Missing modules are errors. An unknown module version is a warning. |
| `NVIDIA device nodes` | Error or warning | Required device nodes exist, including `/dev/nvidiactl`, `/dev/nvidia-uvm`, and at least one `/dev/nvidia<N>` GPU node. Missing required nodes are errors. Missing UVM tools, capability, or DRI render nodes are warnings. |
| `NVIDIA driver libraries` | Error | `ldconfig` can discover required host driver libraries for the host architecture, including `libcuda.so.1` and `libnvidia-ml.so.1`. |
| `NVIDIA diagnostic tooling` | Error or warning | `nvidia-smi -L` can query GPUs. Missing `nvidia-smi` is a warning. A failed or empty query is an error. |
| `NVIDIA persistence service` | Warning | `nvidia-persistence-mode.service` is installed and active when systemd is available. |

### Cluster API Check

| Check | Severity when failing | What it validates |
|---|---|---|
| `api-server-reachable` | Error | Cluster CA data and bootstrap credentials are valid enough to use, the configured API server endpoint is well formed, and the host can reach the API server `/readyz` endpoint. API server responses with status 500 or higher are fatal. |

### Rootfs and Artifact Checks

| Check | Severity when failing | What it validates |
|---|---|---|
| `oci-image-reachable` | Error | The selected rootfs OCI image is configured and its registry manifest, local layout, or HTTPS layout archive is reachable without pulling image contents. |
| `kubernetes-artifacts` | Error | Required Kubernetes binary sources are reachable without downloading full artifacts: kubelet, kubectl, and kube-proxy. |
| `cri-artifacts` | Error | Required CRI artifact sources are reachable without downloading or extracting full artifacts: containerd, runc, and crictl. |
| `cni-artifacts` | Error | The CNI plugins artifact source is reachable without downloading or extracting the full artifact. |
| `nspawn-machine-provisioning` | Error or warning | The nspawn machine directory and provisioning paths are usable. Invalid or unreadable paths are fatal. Existing machine directory permissions that are too restrictive are warnings. |

HTTP artifact checks probe each resolved URL without downloading the full
artifact. OCI registry checks resolve the image manifest without pulling
layers; HTTPS archive checks probe the archive URL without downloading it.

## Sensitive Values

Preflight output must not be treated as a dump of agent configuration. The
report uses logical targets such as `cluster API server`, bootstrap
credentials, `rootfs image`, and `kubernetes artifacts` instead of raw tokens,
certificate data, URLs, image references, or file contents.

## Related Pages

- [Agent Configuration]({{< relref "reference/agent/configuration" >}})
- [systemd-nspawn Isolation]({{< relref "reference/agent/nspawn" >}})
- [Agent Guide]({{< relref "guides/agent" >}})
