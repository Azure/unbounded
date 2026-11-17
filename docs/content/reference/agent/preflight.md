---
title: "Preflight"
weight: 2
description: "Run non-mutating host and configuration checks with unbounded-agent preflight before bootstrapping a node."
---

`unbounded-agent preflight` answers one question: can this host, with this
agent configuration, complete a node bootstrap? It reads the host, resolves the
same machine goal state that `start` builds, and runs a fixed set of checks
against both. Every check returns `ok`, `warning`, or `error`. Warnings describe
conditions that bootstrap normally repairs or tolerates, while errors describe
conditions that stop bootstrap unless you explicitly ignore them.

The command is non-mutating in the sense that it does not install packages,
write systemd units, pull images, create a root filesystem, or migrate the host
directory layout. The writable-path checks do create and immediately remove a
temporary probe file, and the readiness probes open network connections, but
nothing on the host is left changed. That makes preflight safe to run before a
bootstrap, again after a failed bootstrap, and on a host that is already
serving a node.

A useful mental model is a dry run of the first part of `start`: configuration
loading, validation, installation ownership admission, goal-state resolution,
then read-only checks grouped by bootstrap phase. The checks are the same
functions that the agent's host, node-start, and rootfs phases expose, which is
why a failure here usually names the exact phase that would have failed later.

## Usage

Run preflight as root on the node host:

```bash
sudo /opt/unbounded/agent/bin/unbounded-agent preflight \
  --config /etc/unbounded/agent.json
```

The `--config` flag points at the same JSON agent configuration file `start`
uses. When it is set, preflight exports it as `UNBOUNDED_AGENT_CONFIG_FILE` for
the duration of the run and restores the previous value afterward. When it is
not set, configuration loading follows the normal agent precedence: the file
named by `UNBOUNDED_AGENT_CONFIG_FILE` if that variable is set, otherwise the
legacy per-field environment variables. In practice, run preflight with the
same `--config` or environment that you use for `start`, because a check that
passes against one configuration says nothing about another.

The configuration is loaded and validated before any check runs, using the same
loader as `start`. That parity is deliberate: preflight must not accept a file
that `start` rejects, and it must not silently normalize something differently.
Configuration load and validation failures are returned as ordinary command
errors, so they are printed to standard error and no check report is produced.
See [Config Loading and Goal Resolution](#config-loading-and-goal-resolution)
for the failures that happen in this early stage.

The automatic invocation comes from the install script. When you install or
upgrade the agent with `unbounded-agent-install.sh`, the script runs
`unbounded-agent preflight` before `unbounded-agent start` unless you disable
it. Preflight is enabled by default: the script tests `AGENT_PREFLIGHT` with a
default of `true`, and treats `0`, `false`, `no`, `FALSE`, `NO`, `False`, and
`No` as the values that skip the check. Any other value, including an unset
variable, runs it:

```bash
sudo AGENT_PREFLIGHT=false ./unbounded-agent-install.sh
```

Skipping the automatic preflight is reasonable in automation that already ran
the command and wants to avoid doing the work twice. It does not make `start`
more permissive: `start` still enforces its own prerequisites and still refuses
configurations it cannot use.

The privileged-user requirement is part of the check set as well as a practical
requirement for meaningful results. `is-privileged-user` fails when the
effective user ID is not zero. Several other checks read root-owned paths and
inspect `/proc`, and the writable probes only prove what the current user can
do, so running as a non-root user would produce misleading successes or
uninspectable failures.

Repeated runs are expected and supported. On a clean host, preflight sees no
installation record and runs the full check set. If a previous bootstrap was
interrupted, the installation record marks the host as resumable and preflight
skips `existing-deployment`, because the artifacts that check would report as
leftovers belong to the installation being resumed. If the installation record
says bootstrap already completed, preflight returns an empty report instead of
re-checking a host that is already a node:

```text
[preflight] Running unbounded-agent pre-flight checks
```

In JSON mode the same case produces a report with `checks` set to `null`,
because there is nothing to report. This is a successful exit, not an error.

## Flags

Preflight has five flags of its own. The root command's persistent flags,
including `--debug`, `--log-format`, and `--no-color`, also apply; the install
script passes `--debug` through to preflight when `AGENT_DEBUG` enables debug
logging.

| Flag | Default | Accepted values | Effect |
| --- | --- | --- | --- |
| `--config` | empty | Path to a JSON agent configuration file | Sets `UNBOUNDED_AGENT_CONFIG_FILE` for this run and restores it afterward. An empty value falls back to the environment. |
| `--isolation` | empty | `nspawn`, `none`, `container` | Overrides the configuration's `Isolation` for this run. An empty value keeps the configured mode, and an empty configured mode means `nspawn`. `container` also requires `Experimental.ContainerIsolation` in the configuration file. |
| `--ignore-preflight-errors` | empty | Comma-separated check names, or `all` | Downgrades matching error results to warnings and marks them `ignored`. Names are matched case-insensitively after trimming spaces. |
| `--fail-on-warnings` | `false` | Boolean | Makes any remaining warning fail the command, including warnings produced by ignored errors. |
| `--output` | `text` | `text`, `json` | Selects the report format. Matching is case-insensitive. Any other value is an error. |

The isolation override is applied during configuration loading, before
validation, so an invalid value fails exactly like an invalid `Isolation` field
in the file: `invalid --isolation: ...`. The mode changes which backend check
runs and which artifacts are probed, but it does not bypass the checks that
every host must pass.

`--ignore-preflight-errors` takes check names, not targets and not severity
levels. The name is the first field in a text report line, such as
`disk-space` or `nvidia-driver`. One name can produce several results, because
some checks report one result per target. Ignoring the name downgrades every
error result from that check. The special value `all` matches every check name,
so `--ignore-preflight-errors=all` turns every remaining error into a warning.
It does not change targets or messages, and it does not suppress the results
from the report: they still appear, with `ignored` set to `true` in JSON.

An ignored error is still a warning. That distinction matters when
`--fail-on-warnings` is set: ignoring an error downgrades it, and then the
warning policy fails the run because a warning remains. If your goal is to make
the command exit zero while keeping a record of the problem, either fix the
condition or omit `--fail-on-warnings`.

## Exit Behavior

The report decides the exit status. A report is `failed` when it contains at
least one remaining error, or when `--fail-on-warnings` is set and it contains
at least one warning. Otherwise the report is `ok`. The command returns no error
when the report is `ok`, so the process exits `0`. A failed report returns an
error from the command, and the agent exits `1`:

| Condition | Report status | Exit code |
| --- | --- | --- |
| Every check returns `ok` | `ok` | `0` |
| One or more warnings, without `--fail-on-warnings` | `ok` | `0` |
| One or more warnings with `--fail-on-warnings` | `failed` | `1` |
| One or more errors, without ignores | `failed` | `1` |
| Errors downgraded by `--ignore-preflight-errors`, without `--fail-on-warnings` | `ok` | `0` |
| Errors downgraded by `--ignore-preflight-errors`, with `--fail-on-warnings` | `failed` | `1` |
| Configuration, admission, or goal-state failure | No report | `1` |

Warnings are non-fatal by default because many of them describe state that
bootstrap changes. For example, an active Docker unit is a warning because
bootstrap disables it, and a missing host package is a warning because the
package installation task can add it. Errors describe conditions bootstrap does
not repair: it cannot make a read-only filesystem writable, free disk space,
load an NVIDIA kernel module, or remove a foreign kubelet on its own.

Ignoring errors does not remove them from the report. The check still runs,
still reports the condition, and still carries the original message and target.
The only change is severity plus the `ignored` flag. This is intentional: the
output should keep telling you what is wrong even when you have decided to
continue. It also means that a later `--fail-on-warnings` run of the same host
will fail until the condition is fixed, even if the earlier run exited zero.

Failures before the checks run are different. There is no report and no exit
status derived from one, because the command cannot construct a check set. Those
failures are the ones covered in [Config Loading and Goal
Resolution](#config-loading-and-goal-resolution). They exit `1` and print an
error line, which is exactly what `start` does for the same input.

## Text Output

Text is the default format and is meant to be read by a person. It follows the
same shape as kubeadm preflight output: a header, one line per successful or
warning check, and then a summary block for errors.

```text
[preflight] Running unbounded-agent pre-flight checks
	[OK is-privileged-user]: preflight is running as root (target: host user)
	[OK existing-deployment]: no existing node deployment was detected (target: host deployment)
	[WARNING host-packages]: required host packages are missing and may be installed by bootstrap: curl (target: host packages)
	[OK disk-space]: sufficient disk space is available (target: host disk)
```

Each line starts with a tab and a bracketed status. `OK` and `WARNING` results
are printed inline. Error results are not printed inline; they are collected and
printed in a fatal summary after all other lines, so that a failed run ends with
the actionable items instead of burying them among successes:

```text
[preflight] Some fatal errors occurred:
	[ERROR disk-space]: available disk space is below the minimum for /var/lib: current 3.2 GiB, required 8.0 GiB (target: host disk)
[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`
```

The hint line is printed only when at least one remaining error exists. It
reflects the actual mechanism: `--ignore-preflight-errors` takes check names, so
the check name in the `[ERROR ...]` block is the value to pass. Target text, in
parentheses, is there to tell you which object the check examined, such as a
systemd unit, a filesystem path, or a logical artifact name.

A warning that came from an ignored error is printed in the same inline
`[WARNING ...]` form as a native warning. Text output has no separate marker for
`ignored`; use JSON when you need to distinguish the two.

Success and warning lines include the target when it is non-empty. The reporting
layer fills an empty target with the check name, so most lines show one. The
target is a logical label or a host path, never a credential, token, or artifact
URL.

## JSON Output

`--output json` prints the entire report, including successes, as one indented
JSON object. This is the format to use from automation, because it is stable and
because text output omits error lines from their normal position:

```json
{
  "status": "ok",
  "checks": [
    {
      "name": "disk-space",
      "target": "host disk",
      "severity": "ok",
      "message": "sufficient disk space is available",
      "ignored": false
    }
  ],
  "summary": {
    "ok": 1,
    "warnings": 0,
    "errors": 0
  }
}
```

The top-level fields have these meanings:

| Field | Type | Meaning |
| --- | --- | --- |
| `status` | string | `ok` or `failed`, derived from the summary and `--fail-on-warnings`. An empty-installation report has an empty status. |
| `checks` | array or null | Every result from every check, in check order and then result order. `null` when no checks ran. |
| `checks[].name` | string | The check name, suitable for `--ignore-preflight-errors`. |
| `checks[].target` | string | The unit, path, or logical object examined. Defaults to the check name when a result leaves it empty. |
| `checks[].severity` | string | `ok`, `warning`, or `error` after ignore handling. |
| `checks[].message` | string | Human-readable explanation of the outcome. |
| `checks[].ignored` | boolean | True when an error was downgraded by `--ignore-preflight-errors`. |
| `summary.ok` | integer | Number of results with severity `ok`. |
| `summary.warnings` | integer | Number of results with severity `warning`, including ignored errors. |
| `summary.errors` | integer | Number of results with severity `error`. |

The `summary` counts results, not checks. A check can report several results,
as `nvidia-driver` does when it finds problems in more than one part of the GPU
stack, so the summary totals can exceed the number of checks. The `checks` array
is where the detail lives.

`--output` is matched case-insensitively, so `JSON` and `json` behave the same.
An unsupported value is reported as `unsupported output format "..."` after the
checks have run. Nothing is written to the report stream in that case, but the
checks did execute, which is worth remembering if a check has side effects on
the systems it contacts.

## Config Loading and Goal Resolution

Preflight does substantial work before it runs a single check. Understanding
that order explains most of the cases where the command exits non-zero without
printing a report.

The command first loads and validates the agent configuration with the same
loader `start` uses. This applies `--isolation`, normalizes fields such as the
cluster version and API server scheme, resolves the machine name, and validates
required fields. A validation failure is returned as `validate agent config:
...` and no checks run. Typical examples are a missing `MachineName`, an
invalid `NodeName`, a missing `Cluster.ClusterDNS`, a missing or malformed
`Kubelet.ApiServer` when no kubeconfig is supplied, an invalid isolation mode, a
malformed kubelet configuration overlay, unsafe additional host devices or
mounts, or an invalid provisioning format.

Next it builds the bootstrap identity and asks the installation record how to
treat this host. Admission has three outcomes:

| Outcome | When it applies | Effect on preflight |
| --- | --- | --- |
| Fresh | No installation record exists | Run the full check set. |
| Resume | A record exists for the same machine and configuration fingerprint, with bootstrap unfinished | Run the full check set except `existing-deployment`. |
| Already complete | The record says bootstrap completed | Return an empty report and exit successfully. |

Admission also fails before any check when the record cannot be read, when its
contents are invalid, when a reset is incomplete (`reset is incomplete; run
unbounded-agent reset again`), or when the recorded machine name or
configuration fingerprint differs from the current intent (`installation intent
differs; explicit reset is required`). A different fingerprint can come from a
changed Kubernetes version, OCI image, API server, or a non-default isolation
mode, because those are part of the recorded identity.

The identity deliberately excludes credentials and artifact locations so that a
retry can refresh them without looking like a different installation. It is
computed over the cluster version, the canonical OCI image, the API server, and
the mode when the mode is not the default. This is why changing the download
mirror does not force a reset, while changing the API server does.

The command then resolves the download sources, folding in offline artifacts
when `OfflineArtifacts` is configured. This is where an unsupported or
unresolvable download source fails: a bad offline artifact manifest, an
unsupported artifact source scheme, or a missing required artifact entry stops
the run with `resolve download overrides: ...`. Offline artifact resolution also
changes the severity of some checks later, so it has to happen before they run.

Finally it resolves the machine goal state, which is the same structure the
bootstrap phases consume. Resolution reads host inputs such as the kernel,
hostname, distribution, hardware devices, and NVIDIA device state, and produces
the rootfs layout, node-start configuration, and artifact identities. Failures
here stop the run with `resolve goal state: ...`. Examples include an invalid
cluster CA, an invalid node IP, inconsistent bootstrap credentials, an NVIDIA
library discovery failure for an unsupported architecture, or a host whose
kernel or hostname cannot be read.

Bootstrap authentication is validated at this stage rather than during
configuration validation. When a bootstrap token or exec credential is present,
the agent requires exactly one method, and an exec credential must name a
command. If both are set, resolution fails with `kubelet auth: ...`. An empty
authentication block is intentionally allowed here because the attestation flow
can fill in a token later; bootstrapping without either a static credential, an
exec credential, or attestation fails later at kubelet configuration.

NVIDIA resolution is deliberately diagnostic for preflight. `start` resolves
the goal state with NVIDIA readiness enforced, but preflight resolves it without
requiring readiness and without initializing devices. That is what lets the
`nvidia-driver` check report specific problems such as a missing device node or
an unresponsive `nvidia-smi` instead of the whole command failing at resolution
time. The tradeoff is that a successful preflight is not proof that `start` will
accept the GPU state; the bootstrap path enforces readiness independently. See
[NVIDIA driver checks](#nvidia-driver-checks) for the details.

Because all of this happens before the report is created, `--output json` does
not guarantee a JSON document on every run. Automation should treat a non-zero
exit with no parseable output as one of these early failures, and read standard
error for the cause.

## Current Checks

The check set is assembled from three phase packages and flattened in a fixed
order: host checks, then node-start checks, then rootfs checks. Each check has a
stable name. The names are the contract for `--ignore-preflight-errors` and for
JSON consumers, so they are listed in full below.

The host group starts every run. The node-start group checks the listeners and
API server that the node stack needs. The rootfs group checks the artifacts that
will be installed into the node. The mode-specific host check and the
mode-specific rootfs checks vary as described in [Differences by isolation
mode](#differences-by-isolation-mode).

A check normally returns an explicit `ok` result when it ran and found nothing
wrong, so the report shows that it ran. `nvidia-driver` is the exception in
style: it combines several inspections and emits a result only for the parts
that have something to say, plus a single `ok` result when every inspection
passes. The registered check set itself always produces at least one result per
check.

### Host Checks

Host checks verify the parts of the machine that bootstrap does not create:
identity of the process, leftover deployments, installed tools, writable system
paths, systemd services, swap, disk, cgroups, NVIDIA state, and the nftables
capability LocalDNS needs. The fixed order is shown here, with the mode-specific
check inserted between `host-os-configuration` and `docker-active`.

| Check | Target | Severity when it fails | Runs in |
| --- | --- | --- | --- |
| `is-privileged-user` | `host user` | error | All modes |
| `existing-deployment` | Artifact path or machine name | error | All modes, skipped on resume |
| `host-packages` | `host packages` | warning, or error with offline artifacts or when detection fails | All modes |
| `host-os-configuration` | `host OS configuration` | error, or warning when noexec cannot be determined | All modes |
| `nspawn-runtime` | `nspawn runtime` | warning | `nspawn` only |
| `none-backend` | `isolation mode none` | error | `none` only |
| `container-backend` | `isolation mode container (experimental)` | error | `container` only |
| `docker-active` | `docker.service` | warning | All modes |
| `containerd-active` | `containerd.service` | warning | All modes |
| `kubelet-active` | `kubelet.service` | warning | All modes |
| `swap-active` | `host swap` | warning | All modes |
| `disk-space` | `host disk` | error | All modes |
| `cgroups` | `host cgroups` | error | All modes |
| `nvidia-driver` | Several NVIDIA targets | error or warning per target | All modes |
| `localdns-conntrack` | `host conntrack` | error, or warning when nft is absent and not offline | All modes |

If the configuration's isolation mode is invalid, the host group collapses to a
single `isolation-mode` error check instead of guessing a mode. Validation
normally rejects that case before the checks run, so this is a defensive path
rather than something you should expect to see.

#### Privileged user

`is-privileged-user` reads the effective user ID and returns an error when it is
not zero. The fix is to run the command with `sudo` or as root. Running as a
non-root user is not just a policy failure: the writable probes would test the
wrong user's permissions, and several `/proc` and root-owned path inspections
would fail or return incomplete information.

#### Existing deployment

`existing-deployment` enforces the clean-host rule. Bootstrap can only start
from a host that does not already contain node deployment artifacts, because
partial state from an earlier run could be reused accidentally. In `nspawn`
mode, the check looks for registered `kube1` and `kube2` machines, their rootfs
directories under `/var/lib/machines`, their `.nspawn` configuration files,
their systemd service override directories, their bpffs mount paths, their
applied agent configuration, and the agent's own daemon units and recovery
script. In `none` and `container` modes it also looks for that mode's artifacts:

| Mode | Additional artifacts inspected |
| --- | --- |
| `none` | Host-native node directory `/opt/unbounded/node`, plus host-native containerd, kubelet, and LocalDNS units under `/etc/systemd/system`. |
| `container` | Container node directory `/var/lib/unbounded/containers`, plus the container units for `kube1` and `kube2`. |

Every finding is an error that names the artifact and tells you `node reset is
needed before running preflight or start again`. The fix is to run
`unbounded-agent reset` on the host, confirm the listed artifacts are gone, and
then rerun preflight. If an artifact exists but cannot be inspected, the check
reports that instead of silently treating it as absent; the same reset guidance
applies, and you may need to correct the permissions or mount that caused the
inspection failure.

A resumed installation skips this check entirely. On a resumed host, the
artifacts are the ones the interrupted bootstrap created and `start` expects to
find them. The other host checks, including the bind address checks, still run.

#### Host packages

`host-packages` detects how the host supplies required tools and then verifies
that each required tool is present. The package set depends on the isolation
mode and the distribution family:

| Host family | Required packages in `nspawn` | Required packages in `none` and `container` |
| --- | --- | --- |
| Debian (`apt-get`) | `systemd-container`, `curl`, `nftables`, `util-linux` | `curl`, `nftables`, `util-linux` |
| RPM (`tdnf` or `dnf`) | `systemd-container`, `curl`, `nftables`, `util-linux` | `curl`, `nftables`, `util-linux` |

`systemd-container` provides `systemd-nspawn` and `machinectl`, so only the
`nspawn` mode requires it. The other modes run the node stack without a machine
and must not install or demand it. `curl` is used for downloads, `nftables`
provides `nft` for the flush unit and LocalDNS rules, and `util-linux` provides
`mountpoint` for private bpffs cleanup.

When a package is missing, the result is a warning by default, with a message
that bootstrap may install it. The exception is a host with offline artifacts
configured: automatic installation is not available there, so a missing package
is an error that says it cannot be installed automatically. Fix that by ensuring
the host image supplies the package, or by removing `OfflineArtifacts` and
allowing bootstrap to install it.

Host package detection also handles image-managed and capability-only hosts. On
Azure Container Linux, where `/usr` is read-only, the check requires the tools
to be supplied by the image or a system extension and reports an error naming
the missing tools rather than suggesting a package installation that cannot
work. On a host with no package manager at all, the check probes the
capabilities the packages exist to provide (`systemd-nspawn`, `curl`, `nft`,
`mountpoint`). If those resolve, the requirement is satisfied. If detection
fails entirely, `host-packages` reports the detection error, which names the
missing tools, rather than claiming a package manager is required.

#### Host OS configuration

`host-os-configuration` verifies that the agent can write the host paths
bootstrap needs to change. It probes three locations:

| Path | Why it must be writable |
| --- | --- |
| `/etc/sysctl.d` | The agent writes `99-kubernetes.conf` for host sysctls. |
| `/etc/systemd/system` | The agent writes and updates its own systemd units there. |
| The agent install `bin` directory, normally `/opt/unbounded/agent/bin` | The agent installs and runs its own host-side binaries from there. |

The check probes a temporary file in each directory and removes it. For the
agent install directory, which may not exist on a host that has never been
bootstrapped, it probes the nearest existing ancestor. That answers the real
question, which is whether the agent can create the directory, not whether it
already exists. On a host installed by an older release, the planned path can
resolve to the legacy `/usr/local/bin`, so the check follows where the agent
will actually install rather than where a fresh host would.

An unwritable path is an error, not a warning, because installing the bootstrap
binary is the first thing that happens to the host. A read-only `/etc` or
`/etc/systemd/system` would fail bootstrap immediately, and an install directory
on a filesystem mounted `noexec` would let installation succeed but leave
systemd unable to start the daemon. The noexec case produces an explicit error
telling you to remount without `noexec`. If the filesystem flags cannot be
determined, the check warns instead, because it knows it could not answer the
question.

To fix these failures, remount the relevant filesystem read-write, correct the
directory ownership or permissions, or move the agent installation root to a
writable, executable filesystem. If `/opt` or `/usr/local` is read-only because
the image manages it, arrange for the agent installation to live on a writable
filesystem or provide the agent through the image.

#### nspawn runtime

`nspawn-runtime` runs only in `nspawn` mode. It looks for `systemctl`,
`machinectl`, and `systemd-nspawn` on `PATH`, and checks that `/run/systemd/system`
exists, which is how the host reports that systemd is the running init system.
Each missing tool and a missing runtime path are warnings, because bootstrap can
install `systemd-container` on a package-managed host. The fix is to install
that package, or supply the tools through the image on an image-managed host,
and to boot the host with systemd.

The check never treats missing nspawn tooling as success for the mode. It is a
warning because remediation is normally possible, not because the mode can work
without it. If you run with offline artifacts, note that the package warning is
issued independently of `host-packages`; the offline artifact severity rule
applies to `host-packages`, while this check remains a warning.

#### none backend

`none-backend` runs only in `none` mode and delegates to the backend's own
preflight. It refuses a host that already has a kubelet or containerd the agent
does not own. Ownership is judged in three places: a `kubelet.service` or
`containerd.service` unit file in the standard systemd unit directories, an
executable named `kubelet` or `containerd` in the standard binary directories,
or a running process with that name whose executable is not below the agent's
owned base directory. Masked units are not findings, because a unit linked to
`/dev/null` cannot start; this is common on a host that previously ran `nspawn`
mode.

The check also requires a cgroup filesystem, the `systemctl` command, and
writable `/etc`, `/opt/unbounded/node`, and `/var`. A failure names the specific
finding and then recommends the alternative: `use isolation mode nspawn, or
remove the existing kubelet and containerd`. That recommendation is the fix for
the ownership case. For the path case, correct the mount or permissions the
same way as for `host-os-configuration`. The agent never switches modes on its
own in response to this failure.

#### container backend

`container-backend` runs only in `container` mode, which is experimental and
must be enabled in the configuration. It checks that the host can run the
container backend: cgroup v2 must be mounted (the check looks for
`/sys/fs/cgroup/cgroup.controllers`), systemd must be version 254 or newer, and
when SELinux is enforcing, the kernel must accept the label the bundle uses. It
also requires writable `/etc`, `/opt`, and `/var`.

The systemd version requirement exists because the generated unit uses
`DelegateSubgroup=`, `RestartSteps=`, and `RestartMaxDelaySec=`. Older systemd
silently ignores those keys, which would leave runc in the unit's own cgroup and
start the container without the memory and I/O controllers. The failure message
names the version it found and the minimum it needs. Upgrade systemd, or choose
a different isolation mode.

The OCI runtime itself is installed later in bootstrap, so a missing runtime at
`/opt/unbounded/runtime/runc` is tolerated by preflight. A runtime path that
exists but is a directory or is not executable is an error. For the SELinux
failure, use a host whose policy accepts the label, or run with SELinux
permissive or disabled. For writable path failures, fix the mount or
permissions as for the other path checks.

#### Active service warnings

`docker-active`, `containerd-active`, and `kubelet-active` ask systemd for the
`ActiveState` of `docker.service`, `containerd.service`, and `kubelet.service`.
They use `systemctl show --property=ActiveState --value` rather than
`systemctl is-active` because the latter exits non-zero for inactive and missing
units alike, losing the state. An active unit is a warning that bootstrap will
disable it. A state that cannot be determined is also a warning.

These warnings exist because a host that already runs a container runtime or a
kubelet will conflict with the node stack the agent builds: two kubelets or two
container daemons on one host fight over sockets and directories. Bootstrap
disables Docker, containerd, and kubelet, so the warning is informational. If
you need those services running on the same host, do not run Unbounded on it.
If the warning says the state could not be determined, check that systemd is
running and that `systemctl` works in the current environment.

These checks run in every mode, including `none`, even though `none-backend`
independently refuses an unmanaged kubelet or containerd. The duplication is
intentional: the active-unit checks report the same conflict early, with the
check names operators already know, while the backend check performs the deeper
ownership analysis of units, binaries, and processes.

#### Swap

`swap-active` reads `/proc/swaps`. Any line other than the header means swap is
enabled, which produces a warning that bootstrap will disable it. If the file
cannot be read, the check warns that the state could not be determined. The fix
for an unexpected warning is to disable swap yourself (`swapoff -a` and remove
the swap entry from `/etc/fstab`) or accept that bootstrap will do it.

#### Disk space

`disk-space` calls `statfs` on `/var/lib` and compares available bytes, using
the blocks available to unprivileged users and the filesystem block size, with a
minimum of 8 GiB. It is an error when the free space is below the minimum and an
error when the filesystem cannot be inspected, because it cannot prove that
bootstrap has room to unpack a rootfs and install artifacts.

The failure message reports current and required GiB. The fix is to free space
on the filesystem holding `/var/lib`, move `/var/lib` to a larger filesystem, or
grow that filesystem. Do not treat this as a soft limit: rootfs extraction and
other bootstrap state can consume several gigabytes, and running out midway
leaves a partially installed node that requires a reset.

#### Cgroups

`cgroups` verifies that `/sys/fs/cgroup` exists. It is an error when it does
not, because both the node stack and the container backend need a cgroup
filesystem. The fix is to mount cgroup2 at `/sys/fs/cgroup`, which is normally
done by the init system and by the container runtime if the agent itself runs in
a container. On a host where cgroups are not mounted at all, correct the boot
configuration before retrying.

#### NVIDIA driver checks

`nvidia-driver` validates the host NVIDIA stack when NVIDIA hardware is present
and is a no-op success on hosts without it. Hardware detection looks for PCI
devices whose vendor ID is `0x10de` and whose class starts with `0x03` (display
or 3D controller), and for device nodes in `/dev` whose names start with
`nvidia`. If neither is found, the check returns one `ok` result with target
`NVIDIA driver` and message `no NVIDIA GPU hardware detected`. If `/dev` cannot
be inspected and no PCI devices were found, it warns that the device directory
could not be inspected, because it cannot distinguish a CPU-only host from an
unreadable one.

When hardware is present, the check runs six groups of inspections. Each group
can produce its own results, and when at least one finding exists and PCI
devices were detected, the check also prepends an `ok` result for `NVIDIA PCI
hardware` reporting how many PCI devices it found. If every inspection passes,
it returns a single `ok` result for `NVIDIA driver` with message `NVIDIA driver
stack is available`.

| Target | Requirement | Failure severity |
| --- | --- | --- |
| `NVIDIA PCI hardware` | Informational count of detected NVIDIA display or 3D PCI devices. | Not a failure; present only with other findings. |
| `NVIDIA PCI driver` | Every detected NVIDIA display or 3D device is bound to the `nvidia` kernel driver. | error when only some are bound; warning when device nodes exist but no PCI display or 3D device was detected. |
| `NVIDIA kernel modules` | `nvidia`, `nvidia_modeset`, `nvidia_uvm`, and `nvidia_drm` are loaded under `/sys/module`. | error for missing modules; warning when the module version cannot be determined with `modinfo`. |
| `NVIDIA device nodes` | `/dev/nvidiactl`, `/dev/nvidia-uvm`, and at least one `/dev/nvidia<N>` exist. | error for missing required nodes or an unreadable `/dev`; warning for missing `nvidia-uvm-tools`, missing capability nodes, or missing DRI render nodes. |
| `NVIDIA driver libraries` | `ldconfig -p` discovers `libcuda.so.1` and `libnvidia-ml.so.1` for the host architecture, and the reported paths exist. | error when `ldconfig` is missing, fails, or the libraries or their paths are absent. |
| `NVIDIA diagnostic tooling` | `nvidia-smi -L` runs and reports at least one valid GPU enumeration within 20 seconds. | error. |
| `NVIDIA persistence service` | `nvidia-persistence-mode.service` exists and is active. | warning when it is not installed or not active. |

The persistence service is a warning rather than an error because the driver
stack can be usable without it. The other findings are errors because a node
with a provisioned GPU that cannot see it will fail when workloads request the
GPU, often long after bootstrap appears to succeed.

To fix the failures, work from the bottom of the stack upward. Load the missing
kernel modules or install the matching driver package, confirm that every GPU
is bound to `nvidia` (a GPU bound to `nouveau` or unbound is a common cause),
create or repair the device nodes by reloading the driver, refresh the linker
cache after installing libraries, and verify `nvidia-smi -L` reports the GPUs.
The diagnostic message from `nvidia-smi` is included in the failure, and its
guidance is to repair the host driver and verify enumeration before retrying
bootstrap. On a host that previously had working GPUs, a kernel upgrade without
a matching driver rebuild is a frequent cause of all of these findings at once.

The `nvidia-driver` check is diagnostic and does not enforce readiness during
goal-state resolution. `start` resolves the goal state with readiness enforced
and initializes devices on distributions that need it. If the host reports GPUs
but the complete NVIDIA state cannot be described, the bootstrap resolution
fails with `required NVIDIA host state is unavailable` instead of continuing as a
CPU-only node. This is why you may see `preflight` report a specific driver
failure and `start` refuse for the same underlying condition: they examine the
same stack at different points in the process.

#### LocalDNS conntrack

`localdns-conntrack` runs in every mode and returns `ok` immediately when
LocalDNS is disabled. When LocalDNS is enabled, it first looks for `nft`. If
`nft` is absent it warns that nftables will be installed before LocalDNS setup,
unless offline artifacts are configured, in which case it is an error because
nothing will be installed. It then asks `nft --check` to validate a small rule
set that adds a raw-priority output chain with a `notrack` rule for DNS traffic
to `127.0.0.1:53`. If the kernel or nftables does not support that construct, it
reports an error.

This check exists because LocalDNS depends on bypassing conntrack for its local
resolver path. A host whose nftables is too old for raw-priority `notrack`
cannot run LocalDNS correctly. The fix is to upgrade nftables or the kernel, or
disable LocalDNS in the agent configuration. The check is a capability probe
only: it validates the rule with `nft --check` and does not add it to the live
ruleset.

### Cluster API Check

The node-start group checks the two listeners that the node stack will bind and
the API server it will register with. These checks run before the node starts,
so they catch conflicts and connectivity problems while the host is still in a
known state.

| Check | Target | What it requires | Severity when it fails |
| --- | --- | --- | --- |
| `api-server-reachable` | `cluster credentials` or `cluster API server` | The cluster CA decodes, the API server endpoint is a valid URL, and `GET <api-server>/readyz` completes with a status below 500. | error |
| `kubelet-bind-address` | `0.0.0.0:10250` | No foreign TCP listener occupies the kubelet port. | error |
| `containerd-metrics-bind-address` | The resolved containerd metrics address, `0.0.0.0:10257` by default | No foreign TCP listener occupies the containerd metrics port. | error |

`api-server-reachable` first decodes `Cluster.CaCertBase64`; invalid base64
data is reported as `cluster CA data is invalid`. It then requires a non-empty
`Kubelet.ApiServer` and parses it as a URL with a scheme and host. The request
uses the configured CA, added to the system pool, with a 10 second timeout. A
connection failure includes the configured endpoint in the message, and a
response of 500 or higher is a fatal status error. A 4xx response is not a
failure: the check is asking whether something is answering the Kubernetes API
path, not whether the agent has credentials to use it. Credential consistency is
enforced earlier, during goal-state resolution.

The fix for a connectivity failure is to make the API server reachable from the
node host: correct DNS, routing, firewall rules, or the configured endpoint, and
verify that the server's certificate is valid for that endpoint and chains to
the supplied CA. If the CA is invalid, replace `Cluster.CaCertBase64` with the
cluster's base64-encoded CA bundle.

One caveat applies to kubeconfig-based deployments. The check requires
`Kubelet.ApiServer` even when kubelet credentials come from
`Kubelet.KubeconfigData`, because it probes the endpoint directly. If you use a
kubeconfig and preflight reports `API server is required`, set `Kubelet.ApiServer`
to the same server the kubeconfig points at.

The bind address checks parse `/proc/net/tcp` and `/proc/net/tcp6` for a
listening socket on the port, then walk `/proc/<pid>/fd` to identify the owning
process. A listener is accepted as belonging to this installation only when the
process root and executable inode both match the installation's rootfs and
binary paths for the configured mode. On a clean host the rootfs does not exist,
so the check behaves as a plain port test. On a retry after an interrupted
bootstrap, it lets the still-running node keep its ports, which is what makes a
retry possible. A foreign listener is an error that names the process, such as
`kubelet bind address is already in use by process "kubelet" (PID 1234)`. The
fix is to stop the process that owns the port, or choose a host where the port
is free. If the listener cannot be inspected, the check fails because it cannot
prove ownership.

### Rootfs and Artifact Checks

The rootfs group verifies that the images, binaries, and provisioning paths the
node needs are available. It probes sources without pulling image layers or
downloading full artifacts, so it is much cheaper than a bootstrap but still
proves that the URLs and manifests resolve.

| Check | Target | Runs when | Severity when it fails |
| --- | --- | --- | --- |
| `oci-image-reachable` | `rootfs image` | Every mode except `none` | error |
| `kubernetes-artifacts` | `kubernetes artifacts` | Every mode | error |
| `cri-artifacts` | `CRI artifacts` | Every mode | error |
| `cni-artifacts` | `CNI artifacts` | Every mode | error |
| `nspawn-machine-provisioning` | `nspawn machine provisioning` | `nspawn` mode only | error, or warning for restrictive machine directory permissions |
| `localdns-artifact` | `LocalDNS artifact` | When LocalDNS is enabled | error |

`oci-image-reachable` requires a selected OCI image and probes it with the
image-source resolver. A reference to an HTTPS archive is parsed and probed as
an HTTP artifact, an `oci-layout:` reference opens the local layout and resolves
the image manifest, and a registry reference connects to the remote repository
and resolves the manifest. The probe resolves manifests; it does not pull
layers. `none` mode skips this check because it installs release artifacts
rather than unpacking an OS image. `container` mode still pulls an OS image, so
it keeps the check.

A failure means the image reference is malformed, the local layout does not
contain the reference, the registry or archive is unreachable, or no image was
selected at all. Fix the reference, make the registry or archive reachable, or
populate the local layout. The failure intentionally does not echo the image
reference, so check the configuration for the value. If the platform selects a
default image, verify that the default for the host distribution and GPU
presence is the one you expect.

The artifact checks share one implementation. Each builds a set of redacted
source names, probes every source, and reports one error per failing source,
using only the logical name in the message:

| Check | Source names it probes |
| --- | --- |
| `kubernetes-artifacts` | `kubelet`, `kubectl`, `kube-proxy` |
| `cri-artifacts` | `containerd`, `runc`, `crictl` |
| `cni-artifacts` | `cni-plugins` |
| `localdns-artifact` | `coredns` |

Probing a download source depends on its kind. A local path or `file://` URL is
opened and closed, which proves it exists and can be read. An `oci://` source
opens the artifact blob. An HTTP or HTTPS source is probed with `HEAD`, falling
back to a ranged `GET` for the first byte when `HEAD` is unsupported, so the
probe stays lightweight. A failure message looks like `one or more required
Kubernetes artifact sources are not reachable: kubelet`. The fix is to correct
the download override, mirror, or offline artifact bundle for that named
source, and to verify that the host can resolve and reach the endpoint.

When a source set cannot be resolved at all, for example because a version or
URL template is invalid, the check reports `artifact sources could not be
resolved` with no source name. That is a configuration problem rather than a
network problem, and the fix is in `Downloads`, `OfflineArtifacts`, or the
version fields those sources derive from.

`nspawn-machine-provisioning` is the rootfs counterpart of the host machine
checks and runs only in `nspawn` mode. It inspects the resolved machine
directory, its parent, the directory holding the `.nspawn` configuration, and
the directory holding the systemd service override. A missing machine directory
is fine as long as the parent is writable, because bootstrap creates it. An
existing machine directory must be a directory, must be readable, and should
have permissions that allow traversal; overly restrictive permissions produce a
warning because the dbus instance inside the machine needs traversal when an
existing rootfs is reused. A populated machine directory is expected during a
rejoin or a rootfs reuse, and provisioning skips rather than overwrites it.

An unwritable parent or provisioning directory is an error with a message that
names the path that could not be created. Fix the mount or permissions so the
agent can create the machine directory and write the nspawn configuration and
override files. A machine directory that cannot be inspected or read is also an
error; correct the permissions or the mount that made it unreadable.

`localdns-artifact` is appended when LocalDNS is enabled. It resolves the
CoreDNS archive for the configured version and host architecture, then probes
it like any other artifact source. A failure means the CoreDNS source is not
reachable. Fix the CoreDNS download override or the offline bundle, or disable
LocalDNS if you do not need it.

## Differences by isolation mode

The agent supports three isolation modes, and preflight adapts the check set to
each. The default mode is `nspawn`; an empty `Isolation` value and an empty
`--isolation` value both mean `nspawn`. The mode is never auto-detected and is
never changed in response to a failed check.

| Check or behavior | `nspawn` | `none` | `container` |
| --- | --- | --- | --- |
| `systemd-container` in `host-packages` | Required | Not required | Not required |
| Backend host check | `nspawn-runtime` | `none-backend` | `container-backend` |
| `oci-image-reachable` | Runs | Skipped | Runs |
| `nspawn-machine-provisioning` | Runs | Skipped | Skipped |
| `existing-deployment` extra artifacts | None beyond the nspawn set | Host-native node directory and units | Container node directory and units |
| Listener ownership root and executables | Machine rootfs and in-rootfs binaries | Host root and the active version's binaries | Container rootfs and in-rootfs binaries |

In `nspawn` mode, the host runs the agent and the node stack runs inside a
systemd-nspawn machine. Preflight therefore requires the nspawn runtime, checks
the machine rootfs and its provisioning paths, and expects an OS image to pull.
Listener ownership is judged against the machine's rootfs and the binaries
inside it.

In `none` mode, the node stack runs directly on the host using the agent's
versioned binaries. There is no machine and no OS image, so the nspawn runtime
package and the image and machine provisioning checks are omitted.
`none-backend` instead refuses a host that already has a kubelet or containerd
the agent does not own, and requires writable `/etc`, `/opt/unbounded/node`, and
`/var`. Listener ownership is judged against the host root and the executables
under the active version link, so a listener started before an upgrade flip is
still recognized as the agent's own.

In `container` mode, the node stack runs in a privileged OCI container whose
root is the unpacked rootfs of the slot. The OS image is still pulled, so the
image check remains, but no nspawn machine is provisioned. `container-backend`
checks the container prerequisites described above, and the existing-deployment
check adds the container node directory and units. Listener ownership is judged
against the container rootfs. This mode is experimental and requires the
experimental gate in the configuration before validation will accept it.

The mode also affects admission indirectly through the configuration
fingerprint. A non-default mode is part of the recorded identity, so a host
installed in one mode refuses a `start` or `preflight` that presents another
mode until the host is reset. The error is `installation intent differs;
explicit reset is required`, and the fix is to reset the host before changing
modes, not to override the check.

### Differences by host type

Host type changes how the package requirement is satisfied, not which checks
run. On a package-managed Debian or RPM host, `host-packages` detects `apt-get`,
`tdnf`, or `dnf`, queries the installed state of each package, and reports
missing packages as warnings that bootstrap can repair. On Azure Container
Linux, the OS content is image-managed and `/usr` is read-only, so the check
requires the tools to come from the image or a system extension and reports
missing tools as an error instead of proposing an installation that cannot work.
On a host with no package manager, the check falls back to probing the
capabilities the packages provide, so a host that ships `systemd-nspawn`,
`curl`, `nft`, and `mountpoint` directly is accepted.

Offline artifacts are the other host-level modifier. When `OfflineArtifacts` is
configured, `host-packages` turns a missing package into an error because
bootstrap cannot install it, and `localdns-conntrack` turns a missing `nft` into
an error for the same reason. The artifact checks also probe the offline bundle
locations instead of upstream URLs, because the download overrides were resolved
from that bundle before the checks ran.

## Sensitive Values

The report is not a configuration dump. Check messages and targets are written
to be safe to paste into an issue, send to a log aggregator, or return from an
automation endpoint. This is a convention that check implementations follow,
not a redaction pass: a check must not include raw configured values such as
URLs, tokens, certificate data, image references, or file contents in a message
or target.

Artifact checks use logical source names (`kubelet`, `containerd`,
`cni-plugins`, `coredns`) instead of the underlying URLs. The OCI image check
reports `rootfs image` as its target and does not echo the image reference. Host
checks report the path or unit they examined, which is operational context
rather than a secret. Tokens and certificate data are never included: CA
failures describe validity, and credential consistency errors describe the shape
of the configuration without printing it.

There is one deliberate exception. A failed API server connection includes the
configured endpoint in the message, because the endpoint is the single most
useful piece of information for diagnosing connectivity and it is not a
credential. Treat reports as host-internal data when the API server address
itself is sensitive.

The JSON report contains the same message strings as text output, so redaction
is not a property of the text formatter. If you add a check, keep the rule in
mind: name the target descriptively, and do not interpolate configuration
values.

## Related Pages

- [Agent configuration reference]({{< relref "reference/agent/configuration" >}})
  for the fields preflight validates and the modes it accepts.
- [Isolation modes]({{< relref "reference/agent/isolation" >}}) for how
  `nspawn`, `none`, and `container` differ.
- [systemd-nspawn isolation]({{< relref "reference/agent/nspawn" >}}) for the
  machine layout and runtime the nspawn checks expect.
- [Direct placement isolation]({{< relref "reference/agent/isolation-none" >}})
  for what the `none` backend requires from a host.
- [Container isolation]({{< relref "reference/agent/isolation-container" >}})
  for the experimental backend prerequisites.
- [Agent guide]({{< relref "guides/agent" >}}) for installing and running the
  agent on a host.
- [Agent operations]({{< relref "guides/operations/agent-operations" >}}) for
  reset, upgrade, and recovery procedures when a check tells you to reset.
