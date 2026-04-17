#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# WARNING: This script downloads binaries without checksum verification.
# It is intended for development/testing only and should NOT be used in production.
# For production deployments, use verified container images with pinned digests.
#
# bootstrap-node.sh - Bootstrap a node to join a Kubernetes cluster
#
# This script downloads and installs the required Kubernetes components (containerd,
# runc, kubelet, kubectl), then configures the kubelet with TLS bootstrapping to
# join a cluster.
#
# Usage:
#   ./bootstrap-node.sh -k <version> --api-server <hostname:port> --token <token> --ca-cert <path> --cluster-dns <ip>
#
# Required arguments:
#   -k, --kubernetes-version  Kubernetes version to install (e.g., 1.30.0, v1.30.0)
#   --api-server              Kubernetes API server hostname:port (e.g., api.example.com:6443)
#   --token                   Bootstrap token in format [a-z0-9]{6}.[a-z0-9]{16}
#   --ca-cert                 Path to the cluster CA certificate file
#   --cluster-dns             Cluster DNS service IP address (e.g., 10.96.0.10)
#
# Optional arguments:
#   --containerd-version      Containerd version (default: 1.7.24)
#   --runc-version            Runc version (default: 1.2.4)
#   --crictl-version          Crictl version (default: 1.32.0)
#   --node-name               Override the node name (default: short hostname)
#   --kubelet-dir             Kubelet configuration directory (default: /etc/kubernetes)
#   --skip-install            Skip downloading/installing components (configure only)
#   --dry-run                 Print what would be done without making changes
#   --help                    Show this help message
#

set -euo pipefail

# Default values
KUBELET_DIR="/etc/kubernetes"
NODE_NAME=""
DRY_RUN=false
SKIP_INSTALL=false
API_SERVER=""
BOOTSTRAP_TOKEN=""
CA_CERT_PATH=""
KUBE_VERSION=""
CLUSTER_DNS=""
CONTAINERD_VERSION="1.7.24"
RUNC_VERSION="1.2.4"
CRICTL_VERSION="1.32.0"

# Detect architecture
ARCH=$(uname -m)
case $ARCH in
    x86_64)
        ARCH="amd64"
        ;;
    aarch64)
        ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH" >&2
        exit 1
        ;;
esac

# Detected node IPs (set during detect_node_ips)
NODE_IP_V4=""
NODE_IP_V6=""

# Temporary directory for downloads
DOWNLOAD_DIR=""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}

log_step() {
    echo -e "${BLUE}[STEP]${NC} $1"
}

usage() {
    head -29 "$0" | grep '^#' | sed 's/^# *//'
    exit 0
}

die() {
    log_error "$1"
    exit 1
}

cleanup() {
    if [[ -n "$DOWNLOAD_DIR" ]] && [[ -d "$DOWNLOAD_DIR" ]]; then
        rm -rf "$DOWNLOAD_DIR"
    fi
}

trap cleanup EXIT

# Parse command line arguments
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            -k|--kubernetes-version)
                KUBE_VERSION="$2"
                shift 2
                ;;
            --api-server)
                API_SERVER="$2"
                shift 2
                ;;
            --token)
                BOOTSTRAP_TOKEN="$2"
                shift 2
                ;;
            --ca-cert)
                CA_CERT_PATH="$2"
                shift 2
                ;;
            --containerd-version)
                CONTAINERD_VERSION="$2"
                shift 2
                ;;
            --runc-version)
                RUNC_VERSION="$2"
                shift 2
                ;;
            --crictl-version)
                CRICTL_VERSION="$2"
                shift 2
                ;;
            --cluster-dns)
                CLUSTER_DNS="$2"
                shift 2
                ;;
            --node-name)
                NODE_NAME="$2"
                shift 2
                ;;
            --kubelet-dir)
                KUBELET_DIR="$2"
                shift 2
                ;;
            --skip-install)
                SKIP_INSTALL=true
                shift
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            --help|-h)
                usage
                ;;
            *)
                die "Unknown option: $1"
                ;;
        esac
    done
}

# Normalize version string (ensure it has 'v' prefix for Kubernetes)
normalize_kube_version() {
    local version="$1"
    # Remove 'v' prefix if present for consistency, then add it back
    version="${version#v}"
    echo "v$version"
}

# Get version without 'v' prefix
strip_version_prefix() {
    local version="$1"
    echo "${version#v}"
}

# Validate required arguments
validate_args() {
    local errors=0

    if [[ -z "$KUBE_VERSION" ]]; then
        log_error "Missing required argument: -k/--kubernetes-version"
        errors=$((errors + 1))
    else
        KUBE_VERSION=$(normalize_kube_version "$KUBE_VERSION")
        log_info "Kubernetes version: $KUBE_VERSION"
    fi

    if [[ -z "$API_SERVER" ]]; then
        log_error "Missing required argument: --api-server"
        errors=$((errors + 1))
    fi

    if [[ -z "$BOOTSTRAP_TOKEN" ]]; then
        log_error "Missing required argument: --token"
        errors=$((errors + 1))
    elif ! [[ "$BOOTSTRAP_TOKEN" =~ ^[a-z0-9]{6}\.[a-z0-9]{16}$ ]]; then
        log_error "Invalid bootstrap token format. Expected: [a-z0-9]{6}.[a-z0-9]{16}"
        errors=$((errors + 1))
    fi

    if [[ -z "$CA_CERT_PATH" ]]; then
        log_error "Missing required argument: --ca-cert"
        errors=$((errors + 1))
    elif [[ ! -f "$CA_CERT_PATH" ]]; then
        log_error "CA certificate file not found: $CA_CERT_PATH"
        errors=$((errors + 1))
    fi

    if [[ -z "$CLUSTER_DNS" ]]; then
        log_error "Missing required argument: --cluster-dns"
        errors=$((errors + 1))
    fi

    if [[ $errors -gt 0 ]]; then
        echo ""
        usage
    fi

    # Set default node name if not specified (use short hostname)
    if [[ -z "$NODE_NAME" ]]; then
        NODE_NAME=$(hostname -s)
        log_info "Using short hostname as node name: $NODE_NAME"
    fi
}

# Check prerequisites
check_prerequisites() {
    log_step "Checking prerequisites..."

    # Check if running as root
    if [[ $EUID -ne 0 ]] && [[ "$DRY_RUN" == "false" ]]; then
        die "This script must be run as root (or use --dry-run)"
    fi

    # Check for required commands
    local required_cmds=("base64" "openssl" "curl" "tar")
    for cmd in "${required_cmds[@]}"; do
        if ! command -v "$cmd" &> /dev/null; then
            die "Required command not found: $cmd"
        fi
    done

    # Verify CA certificate is valid
    if ! openssl x509 -in "$CA_CERT_PATH" -noout 2>/dev/null; then
        die "Invalid CA certificate: $CA_CERT_PATH"
    fi

    log_info "Prerequisites check passed"
    log_info "Architecture: $ARCH"
}

# Detect primary IPv4 and IPv6 addresses from the default gateway interface
# This ensures kubelet uses stable, explicitly configured IPs instead of autodetection
detect_node_ips() {
    log_step "Detecting node IP addresses..."

    # Find the interface used for the default IPv4 route
    local default_iface_v4
    default_iface_v4=$(ip -4 route show default 2>/dev/null | awk '{print $5}' | head -1)

    if [[ -n "$default_iface_v4" ]]; then
        # Get the primary IPv4 address on that interface (first non-secondary address)
        NODE_IP_V4=$(ip -4 addr show dev "$default_iface_v4" 2>/dev/null | \
            grep -oP 'inet \K[0-9.]+' | head -1)
        if [[ -n "$NODE_IP_V4" ]]; then
            log_info "Detected IPv4 address: $NODE_IP_V4 (interface: $default_iface_v4)"
        else
            log_warn "Could not detect IPv4 address on interface $default_iface_v4"
        fi
    else
        log_warn "Could not detect default IPv4 interface"
    fi

    # Find the interface used for the default IPv6 route
    local default_iface_v6
    default_iface_v6=$(ip -6 route show default 2>/dev/null | awk '{print $5}' | head -1)

    if [[ -n "$default_iface_v6" ]]; then
        # Get the primary global IPv6 address on that interface (prefer non-temporary, non-link-local)
        # Look for addresses with scope global that are not temporary (no 'temporary' or 'dynamic' flag with short lifetime)
        NODE_IP_V6=$(ip -6 addr show dev "$default_iface_v6" scope global 2>/dev/null | \
            grep -v 'temporary' | grep -oP 'inet6 \K[0-9a-f:]+' | head -1)
        if [[ -n "$NODE_IP_V6" ]]; then
            log_info "Detected IPv6 address: $NODE_IP_V6 (interface: $default_iface_v6)"
        else
            log_warn "Could not detect IPv6 address on interface $default_iface_v6"
        fi
    else
        log_info "No default IPv6 route found (IPv6 may not be configured)"
    fi

    # At least one IP must be detected
    if [[ -z "$NODE_IP_V4" ]] && [[ -z "$NODE_IP_V6" ]]; then
        die "Could not detect any node IP addresses"
    fi
}

# Create download directory
setup_download_dir() {
    DOWNLOAD_DIR=$(mktemp -d)
    log_info "Using temporary directory: $DOWNLOAD_DIR"
}

# Download a file with progress
download_file() {
    local url="$1"
    local dest="$2"
    local desc="$3"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would download: $url"
        return 0
    fi

    log_info "Downloading $desc..."
    if ! curl -fsSL --progress-bar -o "$dest" "$url"; then
        die "Failed to download $desc from $url"
    fi
}

# Install runc
install_runc() {
    log_step "Installing runc ${RUNC_VERSION}..."

    local url="https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${ARCH}"
    local dest="$DOWNLOAD_DIR/runc"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would download runc from: $url"
        log_info "[DRY-RUN] Would install runc to: /usr/local/sbin/runc"
        return 0
    fi

    download_file "$url" "$dest" "runc ${RUNC_VERSION}"

    install -m 755 "$dest" /usr/local/sbin/runc
    log_info "Installed runc to /usr/local/sbin/runc"

    # Verify installation
    if /usr/local/sbin/runc --version &>/dev/null; then
        log_info "runc version: $(/usr/local/sbin/runc --version | head -1)"
    fi
}

# Install containerd
install_containerd() {
    log_step "Installing containerd ${CONTAINERD_VERSION}..."

    local url="https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz"
    local tarball="$DOWNLOAD_DIR/containerd.tar.gz"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would download containerd from: $url"
        log_info "[DRY-RUN] Would extract containerd to: /usr/local"
        log_info "[DRY-RUN] Would create containerd configuration"
        log_info "[DRY-RUN] Would install containerd systemd service"
        return 0
    fi

    download_file "$url" "$tarball" "containerd ${CONTAINERD_VERSION}"

    # Extract to /usr/local
    tar -C /usr/local -xzf "$tarball"
    log_info "Extracted containerd to /usr/local"

    # Create containerd configuration directory
    mkdir -p /etc/containerd

    # Generate default configuration with systemd cgroup driver
    /usr/local/bin/containerd config default > /etc/containerd/config.toml

    # Enable systemd cgroup driver for containerd
    sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
    log_info "Configured containerd with systemd cgroup driver"

    # Install systemd service
    local service_url="https://raw.githubusercontent.com/containerd/containerd/v${CONTAINERD_VERSION}/containerd.service"
    download_file "$service_url" "/etc/systemd/system/containerd.service" "containerd systemd service"

    # Reload systemd and enable/start containerd
    systemctl daemon-reload
    systemctl enable containerd
    systemctl start containerd

    # Wait for containerd to be ready
    sleep 2
    if systemctl is-active --quiet containerd; then
        log_info "containerd is running"
        log_info "containerd version: $(/usr/local/bin/containerd --version)"
    else
        log_warn "containerd may not have started correctly. Check: systemctl status containerd"
    fi
}

# Install crictl
install_crictl() {
    log_step "Installing crictl ${CRICTL_VERSION}..."

    local url="https://github.com/kubernetes-sigs/cri-tools/releases/download/v${CRICTL_VERSION}/crictl-v${CRICTL_VERSION}-linux-${ARCH}.tar.gz"
    local tarball="$DOWNLOAD_DIR/crictl.tar.gz"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would download crictl from: $url"
        log_info "[DRY-RUN] Would install crictl to: /usr/local/bin/crictl"
        return 0
    fi

    download_file "$url" "$tarball" "crictl ${CRICTL_VERSION}"

    # Extract to /usr/local/bin
    tar -C /usr/local/bin -xzf "$tarball"
    chmod 755 /usr/local/bin/crictl
    log_info "Installed crictl to /usr/local/bin/crictl"

    # Create crictl configuration to use containerd
    cat > /etc/crictl.yaml <<EOF
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
debug: false
EOF
    log_info "Created crictl configuration at /etc/crictl.yaml"

    # Verify installation
    if /usr/local/bin/crictl --version &>/dev/null; then
        log_info "crictl version: $(/usr/local/bin/crictl --version)"
    fi
}

# Install kubelet and kubectl
install_kubernetes_binaries() {
    log_step "Installing Kubernetes binaries ${KUBE_VERSION}..."

    local version_stripped
    version_stripped=$(strip_version_prefix "$KUBE_VERSION")

    local kubelet_url="https://dl.k8s.io/release/${KUBE_VERSION}/bin/linux/${ARCH}/kubelet"
    local kubectl_url="https://dl.k8s.io/release/${KUBE_VERSION}/bin/linux/${ARCH}/kubectl"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would download kubelet from: $kubelet_url"
        log_info "[DRY-RUN] Would download kubectl from: $kubectl_url"
        log_info "[DRY-RUN] Would install kubelet to: /usr/bin/kubelet"
        log_info "[DRY-RUN] Would install kubectl to: /usr/bin/kubectl"
        return 0
    fi

    # Download kubelet
    download_file "$kubelet_url" "$DOWNLOAD_DIR/kubelet" "kubelet ${KUBE_VERSION}"
    install -m 755 "$DOWNLOAD_DIR/kubelet" /usr/bin/kubelet
    log_info "Installed kubelet to /usr/bin/kubelet"

    # Download kubectl
    download_file "$kubectl_url" "$DOWNLOAD_DIR/kubectl" "kubectl ${KUBE_VERSION}"
    install -m 755 "$DOWNLOAD_DIR/kubectl" /usr/bin/kubectl
    log_info "Installed kubectl to /usr/bin/kubectl"

    # Verify installations
    log_info "kubelet version: $(/usr/bin/kubelet --version)"
    log_info "kubectl version: $(/usr/bin/kubectl version --client --short 2>/dev/null || /usr/bin/kubectl version --client)"
}

# Configure kernel modules and sysctl settings
configure_system() {
    log_step "Configuring system settings..."

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would remove Docker packages if present"
        log_info "[DRY-RUN] Would configure kernel modules: overlay, br_netfilter"
        log_info "[DRY-RUN] Would configure sysctl settings for Kubernetes"
        return 0
    fi

    # Remove Docker packages if installed (they conflict with containerd)
    local docker_pkgs=(docker-ce docker-ce-cli docker-ce-rootless-extras docker-compose docker-compose-plugin)
    local pkgs_to_remove=()
    for pkg in "${docker_pkgs[@]}"; do
        if dpkg -l "$pkg" &>/dev/null; then
            pkgs_to_remove+=("$pkg")
        fi
    done
    if [[ ${#pkgs_to_remove[@]} -gt 0 ]]; then
        log_info "Removing Docker packages: ${pkgs_to_remove[*]}"
        apt-get remove --purge -y "${pkgs_to_remove[@]}" || true
        rm -rf /etc/docker
        log_info "Docker packages removed"
    fi

    # Load required kernel modules
    cat > /etc/modules-load.d/kubernetes.conf <<EOF
overlay
br_netfilter
EOF

    modprobe overlay
    modprobe br_netfilter
    log_info "Loaded kernel modules: overlay, br_netfilter"

    # Configure sysctl settings
    cat > /etc/sysctl.d/99-kubernetes.conf <<EOF
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF

    sysctl --system > /dev/null 2>&1
    log_info "Applied sysctl settings for Kubernetes"

    # Disable swap (if enabled)
    if [[ -n "$(swapon --show 2>/dev/null)" ]]; then
        swapoff -a
        log_info "Disabled swap"
        # Comment out swap entries in fstab
        sed -i '/\sswap\s/s/^/#/' /etc/fstab
        log_info "Commented out swap entries in /etc/fstab"
    else
        log_info "Swap is already disabled"
    fi
}

# Create directory structure
create_directories() {
    local dirs=(
        "$KUBELET_DIR"
        "$KUBELET_DIR/pki"
        "$KUBELET_DIR/manifests"
        "/var/lib/kubelet"
        "/opt/cni/bin"
        "/etc/cni/net.d"
    )

    for dir in "${dirs[@]}"; do
        if [[ "$DRY_RUN" == "true" ]]; then
            log_info "[DRY-RUN] Would create directory: $dir"
        else
            mkdir -p "$dir"
        fi
    done

    if [[ "$DRY_RUN" == "false" ]]; then
        log_info "Created Kubernetes directories"
    fi
}

# Write CA certificate
write_ca_cert() {
    local dest="$KUBELET_DIR/pki/ca.crt"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would copy CA certificate to: $dest"
    else
        cp "$CA_CERT_PATH" "$dest"
        chmod 644 "$dest"
        log_info "Wrote CA certificate to: $dest"
    fi
}

# Generate bootstrap kubeconfig
write_bootstrap_kubeconfig() {
    local kubeconfig_path="$KUBELET_DIR/bootstrap-kubelet.conf"
    local ca_data
    ca_data=$(base64 -w0 < "$CA_CERT_PATH")

    local kubeconfig_content
    kubeconfig_content=$(cat <<EOF
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ${ca_data}
    server: https://${API_SERVER}
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: kubelet-bootstrap
  name: kubelet-bootstrap@kubernetes
current-context: kubelet-bootstrap@kubernetes
preferences: {}
users:
- name: kubelet-bootstrap
  user:
    token: ${BOOTSTRAP_TOKEN}
EOF
)

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would write bootstrap kubeconfig to: $kubeconfig_path"
        echo "--- Bootstrap kubeconfig content ---"
        # Redact the token for security
        echo "$kubeconfig_content" | sed "s/${BOOTSTRAP_TOKEN}/[REDACTED]/g"
        echo "--- End bootstrap kubeconfig ---"
    else
        echo "$kubeconfig_content" > "$kubeconfig_path"
        chmod 600 "$kubeconfig_path"
        log_info "Wrote bootstrap kubeconfig to: $kubeconfig_path"
    fi
}

# Write kubelet configuration
write_kubelet_config() {
    local config_path="/var/lib/kubelet/config.yaml"

    local kubelet_config
    kubelet_config=$(cat <<EOF
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous:
    enabled: false
  webhook:
    cacheTTL: 0s
    enabled: true
  x509:
    clientCAFile: ${KUBELET_DIR}/pki/ca.crt
authorization:
  mode: Webhook
  webhook:
    cacheAuthorizedTTL: 0s
    cacheUnauthorizedTTL: 0s
cgroupDriver: systemd
clusterDNS:
- ${CLUSTER_DNS}
clusterDomain: cluster.local
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
cpuManagerReconcilePeriod: 0s
evictionPressureTransitionPeriod: 0s
fileCheckFrequency: 0s
healthzBindAddress: 127.0.0.1
healthzPort: 10248
httpCheckFrequency: 0s
imageMinimumGCAge: 0s
logging:
  flushFrequency: 0
  options:
    json:
      infoBufferSize: "0"
  verbosity: 0
memorySwap: {}
nodeStatusReportFrequency: 0s
nodeStatusUpdateFrequency: 0s
rotateCertificates: true
runtimeRequestTimeout: 0s
serverTLSBootstrap: true
shutdownGracePeriod: 0s
shutdownGracePeriodCriticalPods: 0s
staticPodPath: ${KUBELET_DIR}/manifests
streamingConnectionIdleTimeout: 0s
syncFrequency: 0s
volumeStatsAggPeriod: 0s
EOF
)

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would write kubelet config to: $config_path"
        echo "--- Kubelet config content ---"
        echo "$kubelet_config"
        echo "--- End kubelet config ---"
    else
        echo "$kubelet_config" > "$config_path"
        chmod 644 "$config_path"
        log_info "Wrote kubelet config to: $config_path"
    fi
}

# Write kubelet environment/flags file
write_kubelet_flags() {
    local flags_path="/var/lib/kubelet/kubeadm-flags.env"

    # Node labels for Azure integration
    # Note: kubernetes.azure.com/cluster is intentionally omitted because bootstrap
    # scripts are only used for secondary-site nodes that don't belong to the
    # primary cluster's Azure resource group.
    local node_labels="kubernetes.azure.com/managed=false,node.kubernetes.io/exclude-from-external-load-balancers=true"

    # Build --node-ip flag with detected addresses
    # This prevents kubelet from autodetecting IPs which can be unstable
    local node_ip_flag=""
    if [[ -n "$NODE_IP_V4" ]] && [[ -n "$NODE_IP_V6" ]]; then
        # Dual-stack: comma-separated IPv4,IPv6
        node_ip_flag="--node-ip=${NODE_IP_V4},${NODE_IP_V6}"
    elif [[ -n "$NODE_IP_V4" ]]; then
        # IPv4 only
        node_ip_flag="--node-ip=${NODE_IP_V4}"
    elif [[ -n "$NODE_IP_V6" ]]; then
        # IPv6 only
        node_ip_flag="--node-ip=${NODE_IP_V6}"
    fi

    local kubelet_flags
    kubelet_flags=$(cat <<EOF
KUBELET_KUBEADM_ARGS="--bootstrap-kubeconfig=${KUBELET_DIR}/bootstrap-kubelet.conf --kubeconfig=${KUBELET_DIR}/kubelet.conf --hostname-override=${NODE_NAME} --node-labels=${node_labels} ${node_ip_flag}"
EOF
)

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would write kubelet flags to: $flags_path"
        echo "--- Kubelet flags content ---"
        echo "$kubelet_flags"
        echo "--- End kubelet flags ---"
    else
        echo "$kubelet_flags" > "$flags_path"
        chmod 644 "$flags_path"
        log_info "Wrote kubelet flags to: $flags_path"
    fi
}

# Write systemd service file
write_systemd_service() {
    local service_path="/etc/systemd/system/kubelet.service"

    local service_content
    service_content=$(cat <<'EOF'
[Unit]
Description=kubelet: The Kubernetes Node Agent
Documentation=https://kubernetes.io/docs/
Wants=network-online.target containerd.service
After=network-online.target containerd.service

[Service]
ExecStart=/usr/bin/kubelet
Restart=always
StartLimitInterval=0
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
)

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would write kubelet service to: $service_path"
    else
        echo "$service_content" > "$service_path"
        chmod 644 "$service_path"
        log_info "Wrote kubelet service to: $service_path"
    fi

    # Write service drop-in for configuration
    local dropin_dir="/etc/systemd/system/kubelet.service.d"
    local dropin_path="$dropin_dir/10-kubeadm.conf"

    local dropin_content
    dropin_content=$(cat <<EOF
[Service]
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
ExecStart=
ExecStart=/usr/bin/kubelet \$KUBELET_CONFIG_ARGS \$KUBELET_KUBEADM_ARGS
EOF
)

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would create drop-in directory: $dropin_dir"
        log_info "[DRY-RUN] Would write kubelet drop-in to: $dropin_path"
        echo "--- Kubelet drop-in content ---"
        echo "$dropin_content"
        echo "--- End kubelet drop-in ---"
    else
        mkdir -p "$dropin_dir"
        echo "$dropin_content" > "$dropin_path"
        chmod 644 "$dropin_path"
        log_info "Wrote kubelet drop-in to: $dropin_path"
    fi
}

# Enable and start kubelet
start_kubelet() {
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would reload systemd daemon"
        log_info "[DRY-RUN] Would enable kubelet service"
        log_info "[DRY-RUN] Would start kubelet service"
    else
        log_info "Reloading systemd daemon..."
        systemctl daemon-reload

        log_info "Enabling kubelet service..."
        systemctl enable kubelet

        log_info "Starting kubelet service..."
        systemctl start kubelet

        # Wait briefly and check status
        sleep 2
        if systemctl is-active --quiet kubelet; then
            log_info "Kubelet is running"
        else
            log_warn "Kubelet may not have started correctly. Check: systemctl status kubelet"
        fi
    fi
}

# Print summary
print_summary() {
    echo ""
    log_info "=========================================="
    log_info "Node Bootstrap Complete"
    log_info "=========================================="
    echo ""
    echo "Installed components:"
    echo "  - runc ${RUNC_VERSION}"
    echo "  - containerd ${CONTAINERD_VERSION}"
    echo "  - crictl ${CRICTL_VERSION}"
    echo "  - kubelet ${KUBE_VERSION}"
    echo "  - kubectl ${KUBE_VERSION}"
    echo ""
    echo "Configuration files created:"
    echo "  - ${KUBELET_DIR}/pki/ca.crt"
    echo "  - ${KUBELET_DIR}/bootstrap-kubelet.conf"
    echo "  - /var/lib/kubelet/config.yaml"
    echo "  - /var/lib/kubelet/kubeadm-flags.env"
    echo "  - /etc/systemd/system/kubelet.service"
    echo "  - /etc/systemd/system/kubelet.service.d/10-kubeadm.conf"
    echo "  - /etc/containerd/config.toml"
    echo "  - /etc/crictl.yaml"
    echo ""
    echo "Node name: $NODE_NAME"
    echo "Node IPs: ${NODE_IP_V4:-none}${NODE_IP_V6:+, $NODE_IP_V6}"
    echo "API server: $API_SERVER"
    echo "Cluster DNS: $CLUSTER_DNS"
    echo ""
    if [[ "$DRY_RUN" == "false" ]]; then
        echo "Next steps:"
        echo "  1. The kubelet is now running and will attempt to bootstrap"
        echo "  2. On the control plane, approve the CSR:"
        echo "     kubectl get csr"
        echo "     kubectl certificate approve <csr-name>"
        echo "  3. Verify the node joined:"
        echo "     kubectl get nodes"
        echo ""
        echo "Note: CNI plugins are NOT installed. Deploy your CNI solution"
        echo "      (e.g., unbounded-net) to enable pod networking."
    else
        echo "[DRY-RUN] No changes were made"
    fi
    echo ""
}

# Main function
main() {
    echo ""
    log_info "Kubernetes Node Bootstrap Script"
    log_info "================================="
    echo ""

    parse_args "$@"
    validate_args
    check_prerequisites
    detect_node_ips

    if [[ "$SKIP_INSTALL" == "false" ]]; then
        setup_download_dir
        configure_system
        install_runc
        install_containerd
        install_crictl
        install_kubernetes_binaries
    else
        log_info "Skipping component installation (--skip-install)"
    fi

    log_step "Configuring kubelet..."
    create_directories
    write_ca_cert
    write_bootstrap_kubeconfig
    write_kubelet_config
    write_kubelet_flags
    write_systemd_service
    start_kubelet
    print_summary
}

main "$@"
