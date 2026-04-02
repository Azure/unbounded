---
name: unbounded-agent-qemu-vm-e2e
description: Use this skill to perform unbounded-agent E2E run with qemu VM from local dev env.
---

## Purpose

Use qemu based VM to verify the `unbounded-agent` node bootstrap/run flows.
This skill is back by `hack/agent/qemu`. Refer to that folder for detailed usages.

### When to use

- When you need to perform unbounded-agent E2E run for changes under `cmd/agent`

## Steps

### Start VM and Obtain VM IP

```bash
# Start the VM (requires sudo on macOS for vmnet-shared networking)
./hack/agent/qemu/vm.sh start

# On macOS the VM gets a real IP via DHCP; it is printed at the end of
# the start output and persisted to .vm/agent-vm.ip:
VM_IP=$(cat .vm/agent-vm.ip)

# On Linux the VM is reachable at localhost on the forwarded SSH port (default 2222):
#   ssh -o StrictHostKeyChecking=no -p 2222 ubuntu@localhost
```

**Notes**:

- Sometime the VM might be running already, check before running. If the user wants
  to start from fresh, stop and clean the artifacts beforehand (see below).

- If `sudo` is required on the host, prompt user with the command to run.

### Stop VM

```bash
# Graceful stop
./hack/agent/qemu/vm.sh stop

# Force stop and remove all artifacts (.vm/ disk, ISO, logs, etc.)
./hack/agent/qemu/vm.sh stop --force --clean
```

### Gather Cluster Configuration

The agent requires a JSON config file to join a cluster. For AKS clusters,
use the bundled helper script to extract the config from a kubeconfig:

```bash
# Write agent config JSON to unbounded-agent-config-dev.json at repo root
./hack/agent/.agents/skills/unbounded-agent-qemu-vm-e2e/scripts/aks-config.sh <kubeconfig> [machine-name]

# Example:
./hack/agent/.agents/skills/unbounded-agent-qemu-vm-e2e/scripts/aks-config.sh ~/.kube/my-aks-config
```

The script writes a JSON file matching the `AgentConfig` schema (see `internal/provision/agent_config.go`):

```json
{
  "MachineName": "agent-vm",
  "Cluster": {
    "CaCertBase64": "...",
    "ClusterDNS": "10.0.0.10",
    "Version": "v1.34.3"
  },
  "Kubelet": {
    "ApiServer": "https://...",
    "BootstrapToken": "<id>.<secret>",
    "Labels": {
      "kubernetes.azure.com/managed": "false"
    }
  }
}
```

**Notes**:

- The script uses the first valid bootstrap token it finds with
  `usage-bootstrap-authentication=true`.
- For non-AKS clusters, write `unbounded-agent-config-dev.json` at the repo root manually.

### Perform Node Start

```bash
# 1. Build the agent binary targeting Linux (the VM is always Linux)
GOOS=linux GOARCH=$(uname -m) go build -o bin/unbounded-agent ./cmd/agent # change GOARCH to match the vm arch

# 2. Get the cluster config (for AKS clusters)
./hack/agent/.agents/skills/unbounded-agent-qemu-vm-e2e/scripts/aks-config.sh <kubeconfig>

# 3. Run the agent inside the VM via SSH (the repo is mounted at /agent via virtio-9p,
#    so the freshly-built binary and config file are already available).
#    On Linux, VM_IP is localhost and add -p 2222.
#    See below examples for commands to run in different scenarios.
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@${VM_IP} \
  "sudo UNBOUNDED_AGENT_CONFIG_FILE=/agent/unbounded-agent-config-dev.json /agent/bin/unbounded-agent start"
```

A successful node join should result in seeing the node registered in the target k8s cluster.

**Notes**:

- Inside the VM, you have root permission.
- The repo is mounted at `/agent` inside the VM — both the binary and config file are available without scp.
- Prompt and confirm with user for the kubeconfig / target cluster to connect before running.
- Refer to `internal/provision/assets/unbounded-agent-install.sh` for other options.
- The node showing with `NotReady` state is fine as long as kubelet / containerd logs are looking correctly.

#### Example Commands

Join to an AKS cluster with debug log level:

```bash
./hack/agent/.agents/skills/unbounded-agent-qemu-vm-e2e/scripts/aks-config.sh /path/to/kubeconfig
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@${VM_IP} \
  "sudo UNBOUNDED_AGENT_CONFIG_FILE=/agent/unbounded-agent-config-dev.json /agent/bin/unbounded-agent start --debug"
```

Capture logs to a file for later analysis (recommended):

```bash
ssh ... "sudo ... /agent/bin/unbounded-agent start --debug" 2>&1 | tee .vm/agent-bootstrap.log
```

### Analyze Bootstrap Logs

After a bootstrap run, use the log analyzer to get a phase duration breakdown:

```bash
# Table output (default)
./hack/agent/.agents/skills/unbounded-agent-qemu-vm-e2e/scripts/analyze-bootstrap-log.py .vm/agent-bootstrap.log

# JSON output
./hack/agent/.agents/skills/unbounded-agent-qemu-vm-e2e/scripts/analyze-bootstrap-log.py .vm/agent-bootstrap.log --json
```

The script parses both pretty and JSON log formats, strips ANSI codes, and prints each phase with its duration, status, and parallel grouping.

### Inspect nspawn container status

Inside the VM, we isolate the kubelet / cri states inside a nspawn based container.
To inspect kubelet / cri states, please use `machinectl shell <machine-name> --pipe <command>`.
