#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Local runner for the agent e2e Kind test.
#
# Handles all setup (Kind cluster, networking, VM, bridge attachment) and
# runs the full linear test sequence end-to-end. Cleans up on exit.
#
# Test flow:
#   1. Start node without Machine CR (agent self-registers)
#   2. Wait for node to become Ready
#   3. Validate Machine CR, daemon, upgrade
#   4. Reset, rejoin, validate again
#
# Prerequisites (Fedora):
#   sudo dnf install -y qemu-system-x86 qemu-img genisoimage iptables docker-ce docker-ce-cli containerd.io
#   sudo systemctl enable --now docker
#   sudo usermod -aG docker $USER  # then log out and back in
#
# Prerequisites (Ubuntu/Debian):
#   sudo apt-get install -y qemu-system-x86 qemu-utils genisoimage iptables
#   Docker: https://docs.docker.com/engine/install/
#
# Also requires: go, kind (v0.29.0+), kubectl
#
# Usage:
#   ./hack/agent/e2e-kind/run-local.sh
#   ./hack/agent/e2e-kind/run-local.sh --verbose   # enable diagnostic output

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
E2E="${REPO_ROOT}/hack/agent/e2e-kind/e2e.py"

# Forward --verbose to e2e.py when passed to this script.
E2E_VERBOSE=""
for arg in "$@"; do
    case "$arg" in
        --verbose) E2E_VERBOSE="--verbose" ;;
        *) echo "[ERROR] Unknown argument: $arg" >&2; exit 1 ;;
    esac
done

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-kind}"
export VM_NAME="${VM_NAME:-agent-e2e}"
export VM_SUBNET="${VM_SUBNET:-192.168.100}"
export VM_IP="${VM_IP:-${VM_SUBNET}.10}"
export AGENT_MACHINE_NAME="${AGENT_MACHINE_NAME:-agent-e2e}"
export AGENT_DEBUG="${AGENT_DEBUG:-}"
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-docker}"

BRIDGE="virbr-e2e"
KIND_CONTAINER="${KIND_CLUSTER_NAME}-control-plane"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { echo "[INFO]  $*"; }
error() { echo "[ERROR] $*" >&2; }

# uses_nft_docker returns 0 if Docker is managing the DOCKER-USER chain
# via native nftables. On these systems (Fedora, Arch, etc.) rules must be
# inserted with the `nft` CLI directly - the iptables-nft compatibility
# shim inserts into a separate table that Docker's native nftables ruleset
# never evaluates.
uses_nft_docker() {
    command -v nft &>/dev/null \
        && sudo nft list chain ip filter DOCKER-USER &>/dev/null 2>&1
}

# setup_forwarding configures iptables/nftables rules so that the VM bridge
# can forward traffic to and from the Docker network. The insertion method
# differs between native-nftables Docker and legacy-iptables Docker.
setup_forwarding() {
    local bridge="$1"

    if uses_nft_docker; then
        info "Detected nftables-managed Docker, inserting rules via nft"
        sudo nft insert rule ip filter DOCKER-USER iifname "${bridge}" accept
        sudo nft insert rule ip filter DOCKER-USER oifname "${bridge}" accept
    else
        info "Detected legacy iptables backend, inserting rules into FORWARD chain"
        sudo iptables -I FORWARD -i "${bridge}" -j ACCEPT
        sudo iptables -I FORWARD -o "${bridge}" -j ACCEPT
    fi

    # Docker may insert raw PREROUTING DROP rules that block non-Docker
    # traffic to container IPs.
    sudo iptables -t raw -I PREROUTING -i "${bridge}" -j ACCEPT 2>/dev/null || true

    # NAT so the VM can reach the internet (for cloud-init apt, etc.).
    sudo iptables -t nat -A POSTROUTING \
        -s "${VM_SUBNET}.0/24" ! -d "${VM_SUBNET}.0/24" -j MASQUERADE 2>/dev/null || true
}

# cleanup_forwarding removes the iptables/nftables rules added by
# setup_forwarding. Best-effort; errors are silenced because the rules
# may already be gone.
cleanup_forwarding() {
    local bridge="$1"

    if uses_nft_docker; then
        # Remove our rules by matching on interface name. nft requires
        # handle numbers for deletion, so we find and delete them by
        # listing handles and grepping for our bridge name.
        for handle in $(sudo nft -a list chain ip filter DOCKER-USER 2>/dev/null \
            | grep "iifname \"${bridge}\"" | awk '{print $NF}'); do
            sudo nft delete rule ip filter DOCKER-USER handle "${handle}" 2>/dev/null || true
        done
        for handle in $(sudo nft -a list chain ip filter DOCKER-USER 2>/dev/null \
            | grep "oifname \"${bridge}\"" | awk '{print $NF}'); do
            sudo nft delete rule ip filter DOCKER-USER handle "${handle}" 2>/dev/null || true
        done
    else
        sudo iptables -D FORWARD -i "${bridge}" -j ACCEPT 2>/dev/null || true
        sudo iptables -D FORWARD -o "${bridge}" -j ACCEPT 2>/dev/null || true
    fi

    sudo iptables -t raw -D PREROUTING -i "${bridge}" -j ACCEPT 2>/dev/null || true
    sudo iptables -t nat -D POSTROUTING \
        -s "${VM_SUBNET}.0/24" ! -d "${VM_SUBNET}.0/24" -j MASQUERADE 2>/dev/null || true
}

cleanup() {
    info "Running cleanup..."
    cleanup_forwarding "${BRIDGE}"
    python3 "$E2E" $E2E_VERBOSE cleanup 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
info "Running preflight checks..."

missing=()
for cmd in docker kind kubectl go qemu-system-x86_64 qemu-img genisoimage python3; do
    if ! command -v "$cmd" &>/dev/null; then
        missing+=("$cmd")
    fi
done

if [[ ${#missing[@]} -gt 0 ]]; then
    error "Required commands not found: ${missing[*]}"
    exit 1
fi

if [[ ! -r /dev/kvm ]]; then
    error "/dev/kvm is not accessible. Enable KVM or add yourself to the kvm group."
    exit 1
fi

if ! docker info &>/dev/null; then
    error "Docker is not running or not accessible. Start Docker and ensure your user is in the docker group."
    exit 1
fi

info "All preflight checks passed"

# ---------------------------------------------------------------------------
# Kind cluster
# ---------------------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
    info "Kind cluster '${KIND_CLUSTER_NAME}' already exists, reusing"
else
    info "Creating Kind cluster '${KIND_CLUSTER_NAME}'..."
    kind create cluster --name "${KIND_CLUSTER_NAME}"
fi

# ---------------------------------------------------------------------------
# Kind networking for VM
# ---------------------------------------------------------------------------
KIND_IP=$(docker inspect "${KIND_CONTAINER}" \
    --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')

if [[ -z "${KIND_IP}" ]]; then
    error "Could not determine Kind control-plane container IP"
    exit 1
fi

info "Kind control-plane IP: ${KIND_IP}"

# Set up forwarding rules so the VM bridge can reach Docker and the internet.
setup_forwarding "${BRIDGE}"

# Patch kindnet so CONTROL_PLANE_ENDPOINT uses the container IP instead
# of the hostname (which is unresolvable from the VM).
info "Patching kindnet DaemonSet..."
PATCH="{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"kindnet-cni\",\"env\":[{\"name\":\"CONTROL_PLANE_ENDPOINT\",\"value\":\"${KIND_IP}:6443\"}]}]}}}}"
kubectl -n kube-system patch daemonset kindnet --type=strategic -p "${PATCH}"
kubectl -n kube-system rollout status daemonset/kindnet --timeout=60s

# ---------------------------------------------------------------------------
# QEMU VM
# ---------------------------------------------------------------------------
python3 "$E2E" $E2E_VERBOSE create-vm

# Attach Kind container to VM bridge via a veth pair so that the VM
# subnet is directly reachable at L2.
info "Attaching Kind container to ${BRIDGE} bridge..."
KIND_PID=$(docker inspect "${KIND_CONTAINER}" --format '{{.State.Pid}}')
sudo ip link delete veth-kind-e2e 2>/dev/null || true
sudo ip link add veth-kind-e2e type veth peer name eth-e2e
sudo ip link set veth-kind-e2e master "${BRIDGE}"
sudo ip link set veth-kind-e2e up
sudo ip link set eth-e2e netns "${KIND_PID}"
sudo nsenter -t "${KIND_PID}" -n ip addr add "${VM_SUBNET}.2/24" dev eth-e2e
sudo nsenter -t "${KIND_PID}" -n ip link set eth-e2e up

# Prevent NetworkManager from detaching the veth from the bridge.
if command -v nmcli &>/dev/null; then
    sudo nmcli device set veth-kind-e2e managed no 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# Install Machine CRD
# ---------------------------------------------------------------------------
python3 "$E2E" $E2E_VERBOSE install-machine-crd

# ---------------------------------------------------------------------------
# Initial join: agent self-registers Machine CR
# ---------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  Phase 1: Initial join (no pre-existing CR)"
echo "============================================"
echo ""

python3 "$E2E" $E2E_VERBOSE run-agent
python3 "$E2E" $E2E_VERBOSE wait-for-node
python3 "$E2E" $E2E_VERBOSE validate-kube-proxy
python3 "$E2E" $E2E_VERBOSE validate-machine-cr-created
python3 "$E2E" $E2E_VERBOSE validate-daemon

# ---------------------------------------------------------------------------
# Upgrade: trigger repave via Machine CR
# ---------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  Phase 2: Upgrade via Machine CR"
echo "============================================"
echo ""

python3 "$E2E" $E2E_VERBOSE trigger-upgrade
python3 "$E2E" $E2E_VERBOSE validate-upgrade
python3 "$E2E" $E2E_VERBOSE wait-for-node
python3 "$E2E" $E2E_VERBOSE validate-kube-proxy
python3 "$E2E" $E2E_VERBOSE validate-workload

# ---------------------------------------------------------------------------
# Reset and rejoin
# ---------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  Phase 3: Reset and rejoin"
echo "============================================"
echo ""

python3 "$E2E" $E2E_VERBOSE reset-agent
python3 "$E2E" $E2E_VERBOSE delete-machine-cr

python3 "$E2E" $E2E_VERBOSE ensure-kind-bridge
python3 "$E2E" $E2E_VERBOSE run-agent
python3 "$E2E" $E2E_VERBOSE wait-for-node
python3 "$E2E" $E2E_VERBOSE validate-kube-proxy
python3 "$E2E" $E2E_VERBOSE validate-machine-cr-created
python3 "$E2E" $E2E_VERBOSE validate-daemon
python3 "$E2E" $E2E_VERBOSE validate-workload

# ---------------------------------------------------------------------------
# Done (cleanup runs via trap)
# ---------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  All tests PASSED"
echo "============================================"
