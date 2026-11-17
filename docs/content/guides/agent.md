---
title: "Agent"
weight: 3
description: "Install the Unbounded agent on a host, generate and inspect a bootstrap payload, run it with sudo, and verify that the host joins its site as a worker node."
---

The Unbounded agent turns a freshly provisioned Linux host into a Kubernetes worker node and then keeps that node reconciled with its `Machine` resource. This guide is the task-oriented path: it shows you how to generate a bootstrap payload with `kubectl unbounded machine manual-bootstrap`, inspect what the payload will do, run it on the host, verify the join, and recover when a step fails. It focuses on the default `nspawn` isolation mode and calls out the `none` and `container` alternatives where the commands differ.

The mental model is simple. A bootstrap payload carries a JSON agent configuration that describes the cluster and the node identity, plus a delivery mechanism that installs the agent binary and runs it. In the default script form those are a configuration heredoc and an install script in one file; the cloud-init and Ignition forms deliver the same configuration and binary in the format their host expects. The agent then runs three phases in order, installs itself as a host daemon, and keeps working after the first boot. Everything you debug later comes back to that sequence and to the small set of files the agent owns on the host.

## How It Works

The `unbounded-agent start` command runs the phases that bootstrap a worker node. It runs them in a fixed order, and every phase decides from the host what still needs to be done rather than trusting a progress marker from an earlier attempt. That property is what makes a retry safe and what makes an interrupted install recoverable.

The **host** phase prepares the operating system. It installs or verifies host packages, sets kernel parameters and modules, prepares the container runtime's host-side prerequisites, configures swap, and takes ownership of the installation. It also leaves an existing nftables ruleset alone, so a retry does not disturb firewall state that is already correct.

The **rootfs** phase resolves the node goal state. It detects the CPU architecture, the host kernel and distribution, and any GPUs, then chooses an OCI root filesystem image and the downloads that go with it. It acquires the rootfs, spawns the `nspawn` machine, and stages the node binaries, including containerd, runc, the CNI plugins, crictl, and the Kubernetes components. On a retry it reuses a rootfs that is already registered to a machine instead of rebuilding it.

The **node-start** phase configures and starts the node stack. It writes the containerd, kubelet, CNI, and local DNS configuration, starts the services in dependency order, and performs TLS bootstrapping with the configured bootstrap token. Kubelet requests a client certificate, and the agent waits for the generated kubeconfig. A rejected or expired token, a CA mismatch, or an unreachable API server fails this wait with a specific error instead of surfacing much later.

When the node is up, the agent installs the daemon unit `unbounded-agent-daemon.service` and records the installation as complete. The daemon is the only long-lived process. It watches the `Machine` object and the `MachineOperation` objects that request host-local work, and it regenerates node configuration when the desired state changes. The agent and daemon always run on the host; the isolation mode changes only where containerd, kubelet, and the rest of the node stack run.

The agent supports three isolation modes. `nspawn` is the default and the most managed option. The node stack runs inside a systemd-nspawn machine whose root filesystem lives under `/var/lib/machines/<machine>`, and the agent provisions most node packages and configuration for you. `none` runs the node stack as agent-owned systemd units directly on the host, which suits hosts that already carry the packages a node needs. `container` runs the node stack in a privileged OCI container and is experimental: the agent refuses it unless the configuration opts in with `--experimental-container-isolation` or the matching `Experimental.ContainerIsolation` field. The differences are covered in [Isolation]({{< relref "reference/agent/isolation" >}}).

| Mode | Where the node stack runs | When to use it |
| --- | --- | --- |
| `nspawn` | Inside a systemd-nspawn machine under `/var/lib/machines` | Default; the agent manages the rootfs and most node packages |
| `none` | Agent-owned systemd units on the host | Hosts that already have the node packages, or where a machine is undesirable |
| `container` | A privileged OCI container supervised by a systemd unit | Evaluation only; requires the experimental opt-in |

## Retrying bootstrap and repairing the daemon

The agent takes installation ownership before it changes the host. That decision is recorded in `/var/lib/unbounded/agent/install-state.json`, together with the machine name, an installation ID, a phase, and a fingerprint of the bootstrap identity. The record deliberately stores no progress. It records only whether an installation is in progress, complete, or being reset, because a claim that a particular stage finished can stop being true without anyone noticing.

A retry with the same identity is admitted. If the record says `installing`, `start` runs the phases again. If the record says `complete`, `start` verifies the installed daemon and its files, repairs them when the check fails, and then reports that the installation was already complete. This is why rerunning the saved payload is the first repair to try for a node whose daemon is missing, disabled, or pointing at a binary that no longer exists.

The fingerprint covers the parts of the request that determine which node stack gets built: the Kubernetes version, the OCI image, the API server endpoint, and any non-default isolation mode. Credentials and artifact locations are not part of it, so you can refresh an expired bootstrap token or move a download mirror and retry the same installation. A change to the machine name, the Kubernetes version, the rootfs image, the API server, or the recorded non-default isolation mode is refused with an error that says an explicit reset is required.

That refusal is a safety property, not an inconvenience. Building a second node stack beside the first is not a valid repair. When you genuinely need to change one of those values, run `unbounded-agent reset`, then bootstrap again. A reset stops and removes both possible nspawn machines, cleans up network interfaces, removes configuration and units, and removes the agent's own files. The reset itself records a `resetting` phase first, so a reset that is interrupted must be finished with `unbounded-agent reset` rather than bypassed with a fresh install.

### What a retry reuses

Each stage inspects the host before it acts. Host preparation skips packages that are already installed, preserves a live nftables ruleset, and leaves swap configuration alone when it already matches. The rootfs phase reuses a machine that is registered from the existing rootfs, and node start does not restart a machine that is already running. Node services are restarted only when their configuration actually changed. This is why a retry after a transient download failure or a network timeout is usually quick rather than a full rebuild.

The applied configuration lives under `/etc/unbounded/agent`, with a file per slot and isolation mode such as `kube1-applied-config.json` and `none-kube1-applied-config.json`. The daemon compares the desired configuration with that record to decide whether the node has drifted. Repairs use the applied configuration rather than the original bootstrap input, because after an ordinary repave the bootstrap input may name a slot that has been retired.

A change that kubelet reads only at registration is a special case. Node labels and taints are passed to kubelet when the node first registers, so changing them on a running node generally requires a repave or a manual node patch, not a daemon restart. The agent can detect the difference, but it cannot make kubelet re-register a node that already exists.

### When ownership blocks a retry

The installation lock is a file under `/run`, and the kernel releases it when the process exits. A leftover lock file therefore does not mean the lock is held, and you should not delete it. A second lifecycle operation waits for up to 30 seconds for the holder to finish and then fails with a message that another operation holds the installation lock. If you see that message while nothing appears to be running, check for a stuck `unbounded-agent start` or `unbounded-agent reset` process before trying again.

If a bootstrap is interrupted, the daemon declines to start rather than fighting the unfinished installation for ownership. It exits with status 69, which the unit treats as success and as a reason not to restart, so the service is inactive until the installation finishes. The correct response is to rerun the saved payload or the same `start` command. Do not mask the condition by editing the unit or by deleting the record.

An ownership record that cannot be read is treated as an error, not as a missing record. The agent will not guess whether a host is fresh when the file exists but is malformed. Inspect `/var/lib/unbounded/agent/install-state.json`, correct the file if you are certain of its contents, or run `reset` and bootstrap again. The record is local to the host disk: a reimaged host comes up with no record and is admitted as a fresh installation, which is exactly what makes a reusable autoscaling payload work.

### Repairing the daemon directly

The normal repair path is to rerun the bootstrap payload. If you no longer have the payload, or you need to repair the host with a different configuration file, run the agent directly as root:

```bash
sudo UNBOUNDED_AGENT_CONFIG_FILE=/root/agent-config.json /opt/unbounded/agent/bin/unbounded-agent start
```

The path in that command is an example. The file must be the JSON configuration for this host, and the machine name and cluster identity in it must produce the same installation fingerprint as the original bootstrap. The agent binary is installed by absolute path and is not expected to be on `PATH`; `/opt/unbounded/agent/bin/unbounded-agent` is the usual location on a current host. Running the binary from a login shell is only for repair and diagnosis. Normal lifecycle work goes through systemd or the saved payload, so the installation lock and the daemon unit stay consistent.

## Configuration

The agent never guesses which configuration to use. It reads the JSON file named by the `UNBOUNDED_AGENT_CONFIG_FILE` environment variable. When that variable is empty, it falls back to a set of legacy per-field environment variables. The cloud-init payload writes `/etc/unbounded/agent/config.json` and exports the variable in `runcmd`. The Ignition payload writes the same file and sets the variable on the first-boot unit. The script variant writes the same JSON to a temporary file under `/tmp` and exports that path instead.

The file describes a desired worker node, not a sequence of commands. It names the Machine, the cluster endpoints and CA, the kubelet identity, the labels and taints to register with, and optionally the isolation mode, OCI image, local DNS cache, and download mirrors. The agent resolves that description into a goal state alongside what it discovers on the host.

| Field | Required | Purpose |
| --- | --- | --- |
| `MachineName` | Yes, after resolution | Name of the `Machine` resource and the installation identity |
| `NodeName` | No | Kubernetes Node name; defaults to the host hostname, then `MachineName` |
| `Cluster.CaCertBase64` | Yes in practice | Base64 PEM of the cluster CA used to verify the API server |
| `Cluster.ClusterDNS` | Yes | ClusterIP of the `kube-dns` service |
| `Cluster.Version` | Yes | Kubernetes version used for node binaries and artifact resolution |
| `Kubelet.ApiServer` | Yes unless a kubeconfig is supplied | API server endpoint kubelet talks to |
| `Kubelet.Auth.BootstrapToken` | Yes unless attestation, exec credentials, or a kubeconfig is used | Bootstrap token in `<token-id>.<token-secret>` form |
| `Kubelet.Labels` | No | Labels applied at node registration, including the site label |
| `Kubelet.RegisterWithTaints` | No | Taints applied at node registration |
| `Kubelet.Configuration` | No | Overlay merged over the agent's baseline kubelet configuration |
| `Isolation` | No | `nspawn` (default), `none`, or `container` |
| `OCIImage` | No | Rootfs image reference or archive; empty uses the built-in image |
| `LocalDNS.Enabled` | No | Enables the nspawn-local CoreDNS cache |
| `Downloads` | No | Per-artifact mirror settings for rootfs binaries |

A generated configuration for a normal token join looks like this. The token and CA are redacted here; a real payload contains both, which is why the file must be readable only by root.

```json
{
  "MachineName": "node-01",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  },
  "Kubelet": {
    "ApiServer": "https://api.example.com:6443",
    "Auth": {
      "BootstrapToken": "<token-id>.<token-secret>"
    },
    "Labels": {
      "unbounded-cloud.io/site": "my-site"
    },
    "RegisterWithTaints": []
  }
}
```

`MachineName` becomes the `Machine` resource name, so it must be a valid Kubernetes DNS subdomain. `Cluster.CaCertBase64` is the base64 PEM of the cluster CA, not the path to a file. `Cluster.ClusterDNS` is the `kube-dns` ClusterIP and becomes kubelet's cluster DNS setting. `Cluster.Version` may be written with or without a leading `v`; the agent strips the prefix during loading.

`Kubelet.ApiServer` may omit the scheme, in which case the agent prefixes `https://`. `Kubelet.Auth.BootstrapToken` is used for TLS bootstrap only; after the node obtains its client certificate, the daemon uses its own controller credentials. `Kubelet.Labels` is where the site label lives. `manual-bootstrap` adds `unbounded-cloud.io/site` automatically unless you already set that key or the deprecated `net.unbounded-cloud.io/site` key in `--node-label`.

{{< callout type="warning" >}}
The configuration contains the cluster CA, a bootstrap token, and any registry credentials you put in kubelet configuration. A saved bootstrap script or cloud-init file contains the same secrets. Keep it readable only by root, and delete it when you no longer need it.
{{< /callout >}}

When `MachineName` is absent, the agent resolves it from `AGENT_MACHINE_NAME` and then from the host hostname. Values from those sources are trimmed and lowercased before validation. An explicitly configured name is used as written and must already be valid, because the agent will not silently rewrite a name an operator set. A derived name that is not a valid DNS subdomain fails the load with the rejected value in the error.

The complete field list, defaults, loading order, and environment variable fallback are in [Configuration]({{< relref "reference/agent/configuration" >}}). Read that page before you hand-edit a file, because JSON decoding ignores unknown fields: a misspelled key does not fail the load, it silently has no effect.

## Running the Agent

The rest of this guide follows one concrete installation: an Ubuntu virtual machine with a public IP address joins an existing site as a worker node. The same steps work for a bare-metal host. The only differences are how you reach the host and how you deliver the payload.

### What You'll Do

You will confirm that the site exists, generate a bootstrap payload named for the host, inspect the configuration and the download settings inside it, run it with `sudo`, and verify that the node and its `Machine` resource appear. You will then see how to reuse the same payload for an autoscaling group, where the machine name is resolved on each instance rather than baked into the payload.

The result is a host that runs the agent daemon, has joined the cluster with TLS bootstrap, and carries the site label that scheduling and networking expect. The node stack runs in an nspawn machine by default. Nothing in the normal flow requires you to edit a file on the host by hand.

### Prerequisites

Before you generate a payload, make sure the following are true.

- Your workstation has a working kubeconfig for the cluster and the `kubectl-unbounded` plugin on `PATH`. The plugin provides `kubectl unbounded`, including `kubectl unbounded machine manual-bootstrap`.
- The target site exists and has a usable bootstrap token. `manual-bootstrap` fails if it cannot find an unexpired token for the site. Check the site with `kubectl get sites` and `kubectl get site my-site -o yaml`. If you have not created a site yet, start with [Getting Started]({{< relref "guides/getting-started" >}}) or [Existing Cluster]({{< relref "guides/existing-cluster" >}}).
- The host is a Linux system with systemd and a writable root filesystem, and you can run commands as root through `sudo` or a root login. The install script refuses to run as a non-root user.
- The host architecture is `amd64` or `arm64`. The install script maps `x86_64` to `amd64` and `aarch64` to `arm64`, and refuses anything else.
- `/var/lib` has at least 8 GiB free. Preflight checks this because the rootfs, container images, and staged artifacts all live there.
- The host can reach the API server on the address the kubeconfig uses, and can resolve the cluster DNS address that will be written into kubelet configuration.
- The host can reach the agent release endpoint, or a mirror you name with `--agent-base-url` or `--agent-url`.
- The host is not already running a node stack the agent does not own. Preflight reports active Docker, containerd, and kubelet units, and a bootstrap may disable services that conflict with the node it is about to build. Use a clean host for a first install.

The default `nspawn` mode also needs the systemd-nspawn runtime tools, including `systemctl`, `machinectl`, and `systemd-nspawn`. Preflight warns when they are missing, and the host phase can install them when the host has a package manager. On an immutable image, install them in the image; an offline artifact source covers rootfs binaries, not host packages.

### Steps

First, confirm that the site exists. `manual-bootstrap` selects the site's newest usable bootstrap token itself; you do not need to copy the token by hand. The command only reads cluster state, so it is safe to run at any time.

```bash
kubectl get site my-site -o yaml
```

Next, generate the payload and save it to a file. Saving it is better than piping it straight into `ssh`, because you can inspect it, rerun it after a transient failure, and keep it for the life of the host.

```bash
kubectl unbounded machine manual-bootstrap node-01 --site my-site > bootstrap-node-01.sh
chmod 600 bootstrap-node-01.sh
```

The first positional argument is the machine name. It must be a valid Kubernetes DNS subdomain because it becomes the `Machine` resource name. Omit it when you are building a reusable payload for many instances, or when you intentionally want the host to resolve its own name. The `--site` flag is required: it selects the site's bootstrap token and adds the site label unless you override the site label with `--node-label`.

Now inspect the payload. The script variant embeds the agent configuration as a JSON heredoc, and you can extract and pretty-print it if `jq` is available:

```bash
sed -n '/^{/,/^}$/p' bootstrap-node-01.sh | jq .
```

Check that `MachineName` is the name you expect, that `Cluster.ClusterDNS` and `Cluster.Version` match the cluster, that `Kubelet.ApiServer` is reachable from the host, and that `Kubelet.Labels` contains `unbounded-cloud.io/site: my-site`. The bootstrap token appears in `Kubelet.Auth.BootstrapToken`; do not paste the file or its output into a ticket. If the token is expired, generate a new payload rather than editing the token by hand.

Run the payload on the host. The script requires root, and it exits with a clear message before changing anything if it is not run as root. Piping it over SSH keeps the file off the host, apart from the temporary configuration file the script itself creates.

```bash
ssh azureuser@203.0.113.10 'sudo bash -s' < bootstrap-node-01.sh
```

If you log in as root directly, the form is `ssh root@203.0.113.10 'bash -s' < bootstrap-node-01.sh`. If you prefer to copy the file first, copy it with `scp`, then run `sudo bash /tmp/bootstrap-node-01.sh` on the host. The agent logs each stage as it runs, and the installed script runs `preflight` before `start` unless `AGENT_PREFLIGHT` is set to a false value.

After the command returns, verify the join from your workstation:

```bash
kubectl get nodes -o wide
kubectl get machines
kubectl get machine node-01 -o yaml
```

The node should be present, and it should become `Ready` once the CNI and other node components are up. The `Machine` should carry the site label, and its status should report the `AgentBootstrapped` condition. If the agent created the `Machine` itself, the object appears as soon as the bootstrap reports status; if a controller created it first, the agent only updates status. Check the labels on both objects because the site label is registered on the Node and copied onto the `Machine`:

```bash
kubectl get node node-01 --show-labels
kubectl get machine node-01 --show-labels
```

You can add ordinary node labels at generation time. Repeat `--node-label` for each key, or set the site label yourself to override the value derived from `--site`:

```bash
kubectl unbounded machine manual-bootstrap node-01 --site my-site \
  --node-label workload=general \
  --node-label unbounded-cloud.io/site=my-site
```

If you need to repair a missing site label on the `Machine` after the fact, label the object directly. Labeling the `Machine` does not relabel the Node; kubelet applies node labels at registration, so repair the Node separately or repave the host.

```bash
kubectl label machine node-01 unbounded-cloud.io/site=my-site --overwrite
kubectl label node node-01 unbounded-cloud.io/site=my-site --overwrite
```

### Reusable bootstrap payloads (VMSS / Auto Scaling)

The machine name argument is optional. When you omit it, the payload does not name a machine. The agent resolves the name on the host at startup, in this order: an explicit `MachineName` in the configuration, then the `AGENT_MACHINE_NAME` environment variable, then the host hostname. Values from the environment and the hostname are trimmed and lowercased before they are validated, and the result must be a valid Kubernetes DNS subdomain because it becomes the `Machine` object name.

That resolution order is what lets one payload serve an entire Azure VMSS or AWS Auto Scaling group. Every instance boots with the same user data, derives a different name, creates or updates its own `Machine`, and records its own installation state on its own disk. Generate the reusable cloud-init payload without a positional name:

```bash
kubectl unbounded machine manual-bootstrap --site my-site --variant cloud-init > user-data.yaml
```

Attach `user-data.yaml` to the scale set or launch template as cloud-init user data. On first boot, cloud-init writes `/etc/unbounded/agent/config.json` with mode `0600`, writes the install script to `/usr/local/bin/unbounded-agent-install.sh`, exports `UNBOUNDED_AGENT_CONFIG_FILE`, and runs the script. The script runs preflight and then `start`.

The host hostname is a good default only when your platform gives every instance a unique one. Some autoscaling services reuse a name, and some image configurations set a fixed hostname. If two instances resolve the same name, they will contend for the same `Machine` resource. Inject `AGENT_MACHINE_NAME` into the first-boot environment when you need a name that is not the hostname, or set the hostname in the image. The agent logs the resolved name and its source at information level, so you can confirm which rule won. On a cloud-init host, look for that line in the first-boot output, usually `/var/log/cloud-init-output.log` or the cloud-init journal. After the daemon starts, use the agent's own log commands for node stack logs.

Keep token lifetime in mind for long-lived groups. A payload embeds the bootstrap token that existed when you generated it, and bootstrap tokens expire. A payload that sits in a launch template for weeks can be stale by the time a scale-out uses it. Regenerate the payload on a schedule, or use the controller-managed provisioning path in [Automation]({{< relref "guides/operations/automation" >}}) when hosts are created continuously.

### Where the agent installs

The agent owns a small, fixed part of the host filesystem. Current installations put their binaries and helper programs under `/opt/unbounded/agent`, with `bin` and `libexec` subdirectories. Configuration lives in `/etc/unbounded/agent`, and installation ownership is recorded in `/var/lib/unbounded/agent/install-state.json`. The node rootfs and the runtime data are larger and live under `/var/lib`.

| Path | Contents |
| --- | --- |
| `/opt/unbounded/agent/bin` | `unbounded-agent`, the blue/green binary links, the lifecycle helper, and the recovery script |
| `/opt/unbounded/agent/libexec` | Host-side helper programs such as the local DNS network helper |
| `/etc/unbounded/agent` | Agent configuration, including `<machine>-applied-config.json` and its checksum |
| `/var/lib/unbounded/agent/install-state.json` | Installation ownership: machine, install ID, phase, and config fingerprint |
| `/var/lib/unbounded` | Staging for the agent archive, offline artifacts, and container images |
| `/var/lib/machines/<machine>` | The nspawn root filesystem for `kube1` or `kube2` |

A script-based bootstrap does not use `/etc/unbounded/agent` for its input. It writes the JSON to a temporary file under `/tmp` and points `UNBOUNDED_AGENT_CONFIG_FILE` at that path. The cloud-init and Ignition variants use the `/etc` path, and `reset` removes temporary configuration files that earlier script runs left behind.

The binary is not expected to be on `PATH`. Units and generated scripts invoke it by absolute path, and the install script downloads the release archive, stages it under `/var/lib/unbounded`, and runs admission from the staged executable. Paths are versioned and can be a link or a moved directory, so do not build them by hand in your own automation. Use the agent's commands or the paths the installation prints.

Treat `/opt/unbounded/agent` as reserved for the agent. Its parent `/opt/unbounded` is shared with staged artifacts and OCI layouts, so the agent creates the parent when it is missing, never changes one that already exists, and never removes it. If you pre-create `/opt/unbounded` as a mount point, the agent uses it and leaves the mount alone. That is useful when `/opt` is a separate volume, but the volume must allow programs to run from it. Preflight fails when the agent install directory is on a filesystem mounted `noexec`, because systemd could not start the daemon from it.

Hosts installed by older releases kept their files under `/usr/local`. On such a host the agent links `/opt/unbounded/agent` at the legacy layout, and later moves the files when no older release is left to roll back to. A move takes more than one daemon start: the first pass copies, verifies, and restarts from the copy, and the second removes the legacy files. An interrupted move finishes on the next start, and a failed verification puts the link back so the host keeps running from the legacy path. If the new root's filesystem is `noexec`, the host stays on the legacy layout rather than moving to a root it cannot run from.

An agent release older than the `/opt` layout only knows `/usr/local`. The install script seeds `/usr/local/bin/unbounded-agent` for such a release, and newer agents remove that lone seed once nothing runs from the legacy root. If `/usr/local/bin` is read-only, the script warns that it cannot seed the binary, and only the older agents are affected. Pin a release that understands `/opt` if you need a read-only `/usr`.

Do not install the agent under both roots yourself. The agent refuses a host that appears to have an installation under both `/usr/local` and `/opt/unbounded/agent`, and it tells you to run `reset` or remove the directory that is in the way. A file at `/opt/unbounded/agent` instead of a directory is also refused. In `nspawn` mode a reset stops and removes both possible machines. In every mode it cleans up network interfaces and removes the agent's files under both roots, the configuration directory, the daemon units, and leftover temporary configuration files. That is why it is the correct starting point when the layout is inconsistent.

### Immutable hosts (read-only /usr)

Image-based hosts such as Azure Container Linux often mount `/usr` read-only and ship no cloud-init. The agent still needs a writable place for its binaries, which is why current releases install under `/opt/unbounded/agent` on a writable root filesystem. If `/usr` is read-only, the host cannot use the legacy `/usr/local` layout and must run a release that understands `/opt`.

The `ignition` variant exists for these hosts. Ignition runs from the initramfs, before any service starts, and it can write the agent configuration, the agent binary, and a first-boot unit without a shell or an operator on the machine. Because Ignition cannot extract an archive, cannot resolve a version, and cannot detect an architecture, every input to this variant is required and must be exact.

```bash
kubectl unbounded machine manual-bootstrap acr-node-01 --site my-site \
  --variant ignition \
  --agent-url https://artifacts.example.com/unbounded-agent-linux-amd64 \
  --agent-sha256 <64-hex-sha256-of-the-binary> > config.ign
```

`--agent-url` must point at the bare `unbounded-agent` binary, not at the release tarball. It must use a scheme Ignition can fetch: `http`, `https`, `tftp`, `s3`, `arn`, or `gs`. `--agent-sha256` is the SHA-256 digest of that binary and must be 64 hexadecimal characters. The release publishes a `checksums.txt` file for each release, so copy the line for the binary rather than computing a digest from a different build.

The generated config writes `/etc/unbounded/agent/config.json` with mode `0600` and the binary at `/opt/unbounded/agent/bin/unbounded-agent` with mode `0755`. It also installs `unbounded-agent-bootstrap.service`, which runs `unbounded-agent preflight` and then `unbounded-agent start`. The unit runs on every boot rather than once, because the installation record already says whether the installation is complete. On a complete host the commands return immediately or repair a daemon that is not running, so a stopped or damaged daemon comes back after a reboot.

The bootstrap unit retries a failed bootstrap instead of giving up. It restarts after 10 seconds, backs off to a five-minute delay, and has no start limit because bootstrap has no later chance to run. On systemd versions with the newer restart settings, the delay steps up to the maximum. Follow the first boot with `journalctl` on the host; the unit name is the one above. `reset` disables and removes the first-boot unit as well as the agent files, so a reset host does not bootstrap itself again from Ignition.

Treat `config.ign` as a secret. It contains the agent configuration, including the bootstrap token and the cluster CA, and the file is often stored in an image definition or a provisioning system. Restrict who can read it, and rotate the token if the file leaks.

### Customizing the agent download

By default the install script tracks the latest published release of `unbounded-agent`. That is convenient for a lab, and it means a new release is picked up without editing a payload. Production installations usually pin a version, and disconnected installations always need a mirror.

| Flag on `manual-bootstrap` | Related variable | Effect |
| --- | --- | --- |
| `--agent-version` | `AGENT_VERSION` | Pin a release tag such as `v0.0.10`; unset tracks the latest release |
| `--agent-base-url` | `AGENT_BASE_URL` | Base URL for release assets; the layout under it must match the release layout |
| `--agent-url` | `AGENT_URL` | Fully qualified tarball URL; overrides the version and base URL. With `--variant ignition` it must name the bare binary |
| `--agent-sha256` | `AGENT_SHA256`, when set externally | Required with `--variant ignition`; verifies the binary digest. Script and cloud-init do not export it from this flag |

The base URL must preserve the release layout: `<base>/latest/download/<asset>` when tracking the latest release, and `<base>/download/<tag>/<asset>` when a version is pinned. Mirroring only the asset files at a flat path will not work. Use `--agent-url` when your mirror has a different layout and you can name the exact tarball.

The install script also honors `AGENT_SHA256` when it is present in the environment, in which case it downloads the tarball to a file, verifies the digest, and fails on a mismatch before extraction. `AGENT_DEBUG` set to `1`, `true`, or `yes` adds `--debug` to the agent invocation. `AGENT_PREFLIGHT` defaults to enabled; set it to `0`, `false`, or `no` to skip the automatic preflight run. These variables control the download and the invocation, not the node configuration.

The same choices can be persisted on the `Machine` resource as `spec.agent.version`, `spec.agent.baseURL`, and `spec.agent.url`. Controllers export them to the host before running the install script, and `kubectl unbounded machine register` exposes equivalent flags. Persisting them on the resource is better than editing a payload by hand when the same host may be reprovisioned later.

### Mirroring the rootfs downloads (air-gapped deployments)

The agent installs five groups of binaries into the rootfs: the Kubernetes components (`kubelet`, `kubectl`, and `kube-proxy`), containerd, runc, the CNI plugins, and crictl. Each group has a base URL, a full URL template, and an optional version override. If you set none of them, the agent uses its compiled-in defaults and downloads from each project's upstream release host. The Kubernetes binaries default to `https://dl.k8s.io`.

| Artifact | Base URL flag | URL template placeholders | Version flag |
| --- | --- | --- | --- |
| Kubernetes | `--kubernetes-base-url` | `version`, `arch`, `binary` | `--kubernetes-binary-version` |
| containerd | `--containerd-base-url` | `version`, `version`, `arch` | `--containerd-version` |
| runc | `--runc-base-url` | `version`, `arch` | `--runc-version` |
| CNI plugins | `--cni-base-url` | `version`, `arch`, `version` | `--cni-version` |
| crictl | `--crictl-base-url` | `version`, `version`, `os`, `arch` | `--crictl-version` |

A Kubernetes mirror must preserve the `<base>/v<ver>/bin/linux/<arch>/` layout, because the default URL builder appends those path elements. The other mirrors must preserve the tag and asset names that their release host uses. A mirror that renames files needs the corresponding `--<artifact>-url` template instead of a base URL.

```bash
kubectl unbounded machine manual-bootstrap node-01 --site my-site \
  --kubernetes-base-url https://mirror.example.com/kubernetes \
  --containerd-base-url https://mirror.example.com/containerd \
  --runc-base-url https://mirror.example.com/runc \
  --cni-base-url https://mirror.example.com/cni \
  --crictl-base-url https://mirror.example.com/cri-tools
```

The version flags are optional. The Kubernetes binary version defaults to the cluster's Kubernetes version, and crictl defaults to the cluster's Kubernetes minor with patch zero. containerd, runc, and CNI default to the versions compiled into the agent. Pin them when your mirror holds a single version and you do not want a later agent release to look for a different one.

These overrides become the `Downloads` block in the agent configuration, with a `Kubernetes`, `Containerd`, `Runc`, `CNI`, or `Crictl` entry, each holding `BaseURL`, `URL`, and `Version`. On the `Machine` resource the same values live under `spec.agent.downloads`. An entry you do not set falls back to the agent's default for that artifact; the entries are independent, so you can mirror only containerd while the Kubernetes binaries come from upstream.

For a fully disconnected installation, `--offline-artifacts-source` points at a complete bundle instead of individual mirrors:

```bash
kubectl unbounded machine manual-bootstrap node-01 --site my-site \
  --offline-artifacts-source /srv/unbounded/artifacts
```

The source may be an absolute path, a `file://` URL, an HTTPS archive, or an `oci://` artifact reference. It is embedded as `OfflineArtifacts.Source`, and it takes precedence over per-artifact download overrides. The string is a strict Go template, so a bundle organized by Kubernetes version can use the cluster version in its path. When offline artifacts are configured, missing host packages are reported as preflight errors because bootstrap cannot rely on a package manager to fetch them.

A successful run logs the host, rootfs, and node-start stages, then reports the daemon installation. A failure names the stage that failed and returns the underlying error, so the first useful line is usually the stage name followed by the specific cause. If a run finishes suspiciously quickly, check the log for a reused rootfs or an already complete installation before you assume that a new binary was downloaded.

## GPU Support

GPU support is part of goal-state resolution, not a separate install step. During the rootfs phase the agent probes the host for NVIDIA and AMD hardware and turns what it finds into device, library, and sysfs grants for the node stack. If the host has no GPU, the checks are a no-op and the node is built for CPU only.

For NVIDIA, the agent discovers the device nodes under `/dev`, the driver libraries, and the helper binaries. In `nspawn` mode it binds the devices into the machine, exposes the driver libraries read-only under `/run/host-nvidia/<index>`, and makes the helper binaries available under `/run/host-nvidia-bin`. The node-start phase then builds a driver root, generates a CDI specification with `nvidia-ctk cdi generate`, writes it to `/etc/cdi/nvidia.yaml`, and configures the containerd NVIDIA runtime handler in CDI mode. Workloads request GPUs through the CDI device names rather than through a hand-written device mount.

For AMD, the agent discovers `/dev/kfd` and the DRM card and render nodes under `/dev/dri`. It exposes those devices to the nspawn machine and mounts the sysfs paths the AMD device plugin reads read-only, so the plugin can discover KFD topology and GPU properties inside the machine. The agent does not install the vendor device plugin. Install the NVIDIA device plugin or GPU operator, or the AMD device plugin, after the node joins, using the method your platform supports.

Hosts in `none` mode use the host's devices directly. The NVIDIA toolkit paths are the host paths by default, and you can override them with `NvidiaToolkit` when the toolkit is installed somewhere unusual. Hosts in `container` mode use the same discovery and CDI flow as `nspawn`, but the mode itself is experimental and gated. The GPU reference pages cover the device and library details for each vendor:

- [GPU]({{< relref "reference/gpu" >}}) for the shared model and scheduling expectations.
- [NVIDIA GPUs]({{< relref "reference/gpu/nvidia" >}}) for the driver stack, CDI, and container runtime behavior.
- [AMD GPUs]({{< relref "reference/gpu/amd" >}}) for KFD, DRM nodes, and sysfs exposure.

If the GPU is not one of the automatically discovered devices, expose it explicitly. `--additional-host-device` accepts an absolute path under `/dev` or a systemd device group such as `char-input`, and `--additional-host-mount` accepts `source[:target][:ro]` for a host path. Prefer discovery for standard GPUs; use the explicit flags for accelerators, fabric devices, or device directories that the agent does not know about.

## Troubleshooting

Start with the first error, not the last line. The agent returns early from the phase that failed, and the last line is often a wrapper that adds context to an earlier, more specific message. A message that begins with `read agent config file` is a path problem, `decode agent config file` is a JSON syntax problem, and `validate agent config` is a rule failure in the file.

### The payload stops before changing the host

Run preflight directly as root with the same configuration the payload uses. The command is non-mutating and prints one line per check, or a JSON report when you ask for one.

```bash
sudo UNBOUNDED_AGENT_CONFIG_FILE=/etc/unbounded/agent/config.json \
  /opt/unbounded/agent/bin/unbounded-agent preflight --output json
```

The report calls out a non-root user, an agent install directory that cannot be written or is on a `noexec` filesystem, an API server that is not reachable, and an NVIDIA driver stack that is missing pieces. Some of those are errors and some are warnings, so read the severity rather than assuming the run will continue. `--ignore-preflight-errors=<check-name>` turns a named error into a warning, and `--fail-on-warnings` turns warnings into a failure. Use the ignore flag only when you understand the check; a `noexec` install root, for example, will still fail later when systemd cannot start the daemon.

If preflight passes and `start` still fails immediately, check the config path and the JSON itself. A exported variable pointing at a file that does not exist, or a file with a trailing comma, fails before any host work begins. The generated payload handles both correctly, so a failure here usually means the payload was edited or the environment was overridden after generation.

### The node does not join

Kubelet joins with TLS bootstrap, so a join failure is usually one of four things: the API server is unreachable from the host, the token is rejected or expired, the token belongs to a different site, or the cluster CA does not match the API server certificate. The agent waits for the generated client kubeconfig, so the failure appears in the node-start stage with the wait named in the error.

Check connectivity from the host to the `Kubelet.ApiServer` address and port. A private API server that is reachable from your workstation is not necessarily reachable from the edge site. Check that the token in the payload is still valid, that it was issued for the site you named, and that the site has not been recreated since the payload was generated. If the CA is wrong, kubelet cannot verify the API server and the error points at certificate verification rather than at the token.

For a node that has started but is not behaving, read the node stack journal and inspect the running containers. The agent resolves the active node from the applied configuration, so these commands work in every isolation mode without you naming a machine.

```bash
sudo /opt/unbounded/agent/bin/unbounded-agent logs --unit kubelet --lines 200
sudo /opt/unbounded/agent/bin/unbounded-agent logs --unit containerd --follow
sudo /opt/unbounded/agent/bin/unbounded-agent exec -- crictl ps
```

If the node object exists but stays `NotReady`, check the CNI plugin and the pod network before you reinstall the agent. A node with a working kubelet but no working pod network reports `NotReady` with a network-related condition, and reinstalling the agent is not a CNI repair.

### The daemon is not running

Check the unit and its journal before you touch the installation record:

```bash
systemctl status unbounded-agent-daemon.service
journalctl -u unbounded-agent-daemon.service -n 200
```

If the unit is inactive and the journal shows exit status 69, an installation is unfinished. Rerun the saved payload or the same `start` command with the configuration file. Do not reset the failure state and force a start; the daemon is deliberately standing down so it does not fight the bootstrap for ownership.

If the unit is enabled but failing for another reason, rerun `start` first. It verifies the installed daemon and its files and repairs a missing or damaged daemon when the installation record says `complete`. If the repair fails, read the error before running `reset`; a reset destroys the node stack and is a much larger action than reinstalling a unit.

### The host joined with the wrong name

The machine name is the `Machine` resource name and the installation identity. If a reusable payload produced a name you did not expect, the host hostname probably won. Check the agent's information-level log line for the resolved name and its source: `config`, `env`, or `hostname`. Inject `AGENT_MACHINE_NAME` when the hostname is not suitable, or set the hostname before the payload runs.

A name that is not a valid Kubernetes DNS subdomain fails at startup. Uppercase letters in a derived name are lowercased for you, but underscores, spaces, and names longer than the DNS limit are not repaired. Set a valid name explicitly rather than relying on a hostname that violates the rule.

### A GPU is missing from the node

Check the host first. For NVIDIA, `nvidia-smi` should list the GPUs and the `/dev/nvidia*` device nodes should exist. For AMD, `/dev/kfd` and the `/dev/dri` card or render nodes should exist and the `amdgpu` driver should be loaded. Preflight reports an incomplete driver stack when it finds hardware but cannot find the libraries, device nodes, or services it needs.

Inside the node, confirm that the device plugin is installed and that it can see the devices the agent exposed. The agent prepares the devices, the library paths, and the CDI specification; it does not install the vendor plugin or create GPU resource names for you. If the plugin is running but reports no devices, compare its logs with the grants the agent recorded in the applied configuration and the machine unit.

### Configuration changes seem to do nothing

JSON decoding ignores unknown fields, so a misspelled key is accepted and has no effect. Compare a hand-edited file with the field table in [Configuration]({{< relref "reference/agent/configuration" >}}), and check the applied configuration under `/etc/unbounded/agent` to see what the agent actually recorded. A change that kubelet applies only at registration, such as node labels or taints, also will not take effect on a restart. Repave the host or patch the node object when that is the behavior you need.

## See Also

- [Getting Started]({{< relref "guides/getting-started" >}}) for a first cluster and the controller-managed path to a node.
- [Existing Cluster]({{< relref "guides/existing-cluster" >}}) for connecting Unbounded to a control plane you already run.
- [Agent Reference]({{< relref "reference/agent" >}}) for the agent's files, units, bootstrap phases, and daemon behavior.
- [Configuration]({{< relref "reference/agent/configuration" >}}) for every field, default, and environment variable.
- [Preflight]({{< relref "reference/agent/preflight" >}}) for the checks and how to read their reports.
- [Isolation]({{< relref "reference/agent/isolation" >}}) for choosing between `nspawn`, `none`, and `container`.
- [nspawn]({{< relref "reference/agent/nspawn" >}}) for the default rootfs layout, grants, and lifecycle.
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}}) for upgrades, reboots, resets, and day-two recovery.
- [CLI Reference]({{< relref "reference/cli" >}}) for the complete command surface, including the agent subcommands.
