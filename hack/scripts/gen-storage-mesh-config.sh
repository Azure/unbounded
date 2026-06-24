#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# gen-storage-mesh-config.sh -- Generate a test unbounded-storage TOML config
# for one node of the current Kubernetes cluster, wiring every other node in as
# a TCP peer (a full peer mesh) using each node's InternalIP.
#
# Usage:
#   hack/scripts/gen-storage-mesh-config.sh [options]
#
# Options:
#       --context NAME       kubeconfig context (defaults to current context)
#   -l, --selector SEL       node label selector passed to kubectl get nodes
#                            (default: kubernetes.azure.com/mode=user,
#                            unbounded-cloud.io/unbounded-net-gateway!=true --
#                            user-mode nodes only, excluding net gateways)
#       --local-node NAME    node this config is for (defaults to the local
#                            machine's hostname)
#       --port PORT          fabric / peer TCP port (default 7000)
#       --frontend-port PORT frontend bind port (default 9000)
#       --metrics-port PORT  Prometheus metrics exporter bind port (default 9100)
#       --origin HOST:PORT   s3 backend origin endpoint as host:port, no scheme
#                            (default: ClusterIP of the orca service on the
#                            --orca-port; the backend speaks plaintext HTTP/1.1)
#       --orca-namespace NS  namespace of the orca s3 origin service
#                            (default unbounded-kube)
#       --orca-service NAME  name of the orca s3 origin service (default orca)
#       --orca-port PORT     port of the orca s3 origin service (default 8443)
#   -o, --output PATH        write the config to PATH (default
#                            /etc/unbounded-storage/config.toml; use '-' for
#                            stdout)
#   -h, --help               Show this help and exit
#
# kubeconfig: kubectl's default kubeconfig is used. If it cannot reach the API
# server, the script falls back to /var/lib/kubelet/kubeconfig (useful when
# running directly on a cluster node).

set -euo pipefail

# ── helpers ──────────────────────────────────────────────────────────────────

die() {
	echo "error: $*" >&2
	exit 1
}

usage() {
	# Print the leading "# Usage:" comment block with the "# " prefix stripped.
	# The quit-check runs before the substitution: stripping the "# " leaves a
	# leading space, so testing /^[^#]/ after the substitution would wrongly match
	# every stripped line and quit after the first.
	sed -n '/^# Usage:/,/^[^#]/{ /^[^#]/q; s/^# \{0,1\}//; p }' "$0"
	exit 0
}

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "$1 not found. $2"
}

# ── argument parsing ──────────────────────────────────────────────────────────

OPT_CONTEXT=""
# Default to user-mode nodes only, excluding unbounded-net gateway nodes. The
# "!=true" form also matches nodes lacking the gateway label entirely.
OPT_SELECTOR="kubernetes.azure.com/mode=user,unbounded-cloud.io/unbounded-net-gateway!=true"
OPT_LOCAL_NODE=""
OPT_PORT="7000"
OPT_FRONTEND_PORT="9000"
OPT_METRICS_PORT="9100"
# Empty means "derive the s3 origin from the orca service ClusterIP" (see the
# ClusterIP lookup below). Set --origin host:port to point at a different
# S3-compatible origin. The backend speaks plaintext HTTP/1.1 to a resolved
# IPv4, so the endpoint must be host:port (no scheme).
OPT_ORIGIN=""
OPT_OUTPUT="/etc/unbounded-storage/config.toml"
# Orca S3 origin service the storage backend reads from when --origin is not
# given. DNS is not configured on the host, so the service's ClusterIP is
# resolved via the API server and written into the config as a literal IP.
OPT_ORCA_NAMESPACE="unbounded-kube"
OPT_ORCA_SERVICE="orca"
OPT_ORCA_PORT="8443"

while [[ $# -gt 0 ]]; do
	case "$1" in
	--context)
		OPT_CONTEXT="$2"
		shift 2
		;;
	-l | --selector)
		OPT_SELECTOR="$2"
		shift 2
		;;
	--local-node)
		OPT_LOCAL_NODE="$2"
		shift 2
		;;
	--port)
		OPT_PORT="$2"
		shift 2
		;;
	--frontend-port)
		OPT_FRONTEND_PORT="$2"
		shift 2
		;;
	--metrics-port)
		OPT_METRICS_PORT="$2"
		shift 2
		;;
	--origin)
		OPT_ORIGIN="$2"
		shift 2
		;;
	--orca-namespace)
		OPT_ORCA_NAMESPACE="$2"
		shift 2
		;;
	--orca-service)
		OPT_ORCA_SERVICE="$2"
		shift 2
		;;
	--orca-port)
		OPT_ORCA_PORT="$2"
		shift 2
		;;
	-o | --output)
		OPT_OUTPUT="$2"
		shift 2
		;;
	-h | --help) usage ;;
	*) die "unknown option: $1. Use --help for usage." ;;
	esac
done

# ── preflight checks ──────────────────────────────────────────────────────────

require_cmd kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/"

[[ "$OPT_PORT" =~ ^[0-9]+$ ]] || die "--port must be a number (got '$OPT_PORT')."
[[ "$OPT_FRONTEND_PORT" =~ ^[0-9]+$ ]] || die "--frontend-port must be a number (got '$OPT_FRONTEND_PORT')."
[[ "$OPT_METRICS_PORT" =~ ^[0-9]+$ ]] || die "--metrics-port must be a number (got '$OPT_METRICS_PORT')."
[[ "$OPT_ORCA_PORT" =~ ^[0-9]+$ ]] || die "--orca-port must be a number (got '$OPT_ORCA_PORT')."

# ── kubectl context args ──────────────────────────────────────────────────────

KUBECTL_CTX_ARGS=()
if [[ -n "$OPT_CONTEXT" ]]; then
	KUBECTL_CTX_ARGS=(--context "$OPT_CONTEXT")
fi

KUBECTL_SEL_ARGS=()
if [[ -n "$OPT_SELECTOR" ]]; then
	KUBECTL_SEL_ARGS=(-l "$OPT_SELECTOR")
fi

# ── resolve a working kubeconfig ──────────────────────────────────────────────

# Fall back to the kubelet's kubeconfig when the default one kubectl would use
# can't reach the API server. This lets the script run on a cluster node that
# has no admin kubeconfig of its own but does have /var/lib/kubelet/kubeconfig.
KUBELET_KUBECONFIG="/var/lib/kubelet/kubeconfig"

# Args identifying which kubeconfig to use; empty means kubectl's default
# resolution ($KUBECONFIG or ~/.kube/config).
KUBECTL_CFG_ARGS=()

# Cheap API-server connectivity probe against the currently selected config.
api_reachable() {
	kubectl "${KUBECTL_CFG_ARGS[@]}" "${KUBECTL_CTX_ARGS[@]}" \
		get --raw='/readyz' >/dev/null 2>&1
}

if ! api_reachable; then
	if [[ -r "$KUBELET_KUBECONFIG" ]]; then
		echo "warning: default kubeconfig cannot reach the API server; falling back to $KUBELET_KUBECONFIG" >&2
		# The kubelet kubeconfig has its own context; an explicit --context from
		# the default config won't exist there, so drop it for the fallback.
		KUBECTL_CTX_ARGS=()
		KUBECTL_CFG_ARGS=(--kubeconfig "$KUBELET_KUBECONFIG")
		api_reachable || die "API server unreachable with both the default kubeconfig and $KUBELET_KUBECONFIG."
	else
		die "API server unreachable with the default kubeconfig and $KUBELET_KUBECONFIG is not readable."
	fi
fi

# ── fetch node name / InternalIP pairs ────────────────────────────────────────

# One "<name> <internal-ip>" line per node. Nodes without an InternalIP emit a
# trailing-space line and are rejected below so a missing address fails loudly
# rather than silently producing a peer with an empty address.
NODE_LINES=$(kubectl "${KUBECTL_CFG_ARGS[@]}" "${KUBECTL_CTX_ARGS[@]}" get nodes "${KUBECTL_SEL_ARGS[@]}" \
	-o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{end}{"\n"}{end}' |
	grep -v '^[[:space:]]*$' |
	sort)

[[ -z "$NODE_LINES" ]] && die "no nodes found in the current cluster."

# Parse into parallel arrays of names and IPs, assigning ids 1..N by sort order.
NAMES=()
IPS=()
while IFS=' ' read -r name ip; do
	[[ -z "$name" ]] && continue
	[[ -z "$ip" ]] && die "node '$name' has no InternalIP address."
	NAMES+=("$name")
	IPS+=("$ip")
done <<<"$NODE_LINES"

NODE_COUNT="${#NAMES[@]}"

# ── pick the local node ───────────────────────────────────────────────────────

# Default the local node to this machine's hostname when not given explicitly,
# so the script picks the right peer config for the host it runs on.
if [[ -z "$OPT_LOCAL_NODE" ]]; then
	OPT_LOCAL_NODE="${HOSTNAME:-}"
	[[ -z "$OPT_LOCAL_NODE" ]] && command -v hostname >/dev/null 2>&1 && OPT_LOCAL_NODE="$(hostname)"
	[[ -z "$OPT_LOCAL_NODE" ]] && die "could not determine local hostname; pass --local-node <name> explicitly."
fi

LOCAL_IDX=-1
for i in "${!NAMES[@]}"; do
	if [[ "${NAMES[$i]}" == "$OPT_LOCAL_NODE" ]]; then
		LOCAL_IDX="$i"
		break
	fi
done
[[ "$LOCAL_IDX" -lt 0 ]] && die "node '$OPT_LOCAL_NODE' not found among the $NODE_COUNT selected node(s)."

LOCAL_NAME="${NAMES[$LOCAL_IDX]}"
LOCAL_IP="${IPS[$LOCAL_IDX]}"
LOCAL_ID=$((LOCAL_IDX + 1))

# ── resolve the s3 origin endpoint ────────────────────────────────────────────

# When --origin is not given, point the s3 backend at the orca service. DNS is
# not configured on the host, so resolve the service's ClusterIP via the API
# server and write a literal "<ip>:<port>" endpoint (the backend dials a
# resolved IPv4 over plaintext HTTP/1.1, so no scheme is used).
if [[ -z "$OPT_ORIGIN" ]]; then
	ORCA_CLUSTER_IP=$(kubectl "${KUBECTL_CFG_ARGS[@]}" "${KUBECTL_CTX_ARGS[@]}" \
		get service "$OPT_ORCA_SERVICE" -n "$OPT_ORCA_NAMESPACE" \
		-o jsonpath='{.spec.clusterIP}' 2>/dev/null) ||
		die "could not look up service '$OPT_ORCA_NAMESPACE/$OPT_ORCA_SERVICE'."
	[[ -z "$ORCA_CLUSTER_IP" || "$ORCA_CLUSTER_IP" == "None" ]] &&
		die "service '$OPT_ORCA_NAMESPACE/$OPT_ORCA_SERVICE' has no ClusterIP (got '${ORCA_CLUSTER_IP:-<empty>}'). Pass --origin host:port explicitly."
	OPT_ORIGIN="${ORCA_CLUSTER_IP}:${OPT_ORCA_PORT}"
fi

# ── emit the config ───────────────────────────────────────────────────────────

# Sample sizing for the file-backed test disk and origin backend stripe.
DISK_SIZE=$((2 * 1024 * 1024 * 1024)) # 2 GiB
STRIPE_SIZE=$((4 * 1024 * 1024)) # 4 MiB

# Resolve where the config is written: a file path (default) or stdout ('-').
# For a file path, ensure the parent directory exists and emit a confirmation
# on stderr so stdout stays clean.
if [[ "$OPT_OUTPUT" == "-" ]]; then
	exec 3>&1
else
	out_dir="$(dirname "$OPT_OUTPUT")"
	mkdir -p "$out_dir" || die "could not create output directory '$out_dir'."
	exec 3>"$OPT_OUTPUT" || die "could not open '$OPT_OUTPUT' for writing."
fi

cat >&3 <<EOF
# Generated by hack/scripts/gen-storage-mesh-config.sh -- test config, not for
# production. This file is the unbounded-storage config for node:
#
#   name:        $LOCAL_NAME
#   internal ip: $LOCAL_IP
#   node id:     $LOCAL_ID  (of $NODE_COUNT nodes; ids assigned by sorted name)
#
# Every other node in the cluster is wired into the p2p neighborhood below as a
# TCP peer. Regenerate the peer config for a different node with --local-node
# <name>.

[[backends]]
name = "origin"

[backends.config.s3]
url = "$OPT_ORIGIN"
stripe_size_bytes = $STRIPE_SIZE

[[neighborhoods]]
name = "p2p"
source = "origin"
local_node_id = $LOCAL_ID
EOF

# Peers: every node except the local one.
for i in "${!NAMES[@]}"; do
	[[ "$i" -eq "$LOCAL_IDX" ]] && continue
	peer_id=$((i + 1))
	cat >&3 <<EOF

[[neighborhoods.peers]]
# peer node: ${NAMES[$i]}
id = $peer_id

[neighborhoods.peers.config.tcp]
addr = "${IPS[$i]}:$OPT_PORT"
EOF
done

cat >&3 <<EOF

[[caches]]
name = "cache"
source = "p2p"
disk_pool = "default"

[[disk_pools]]
name = "default"

[[disk_pools.disks]]
page_size_bytes = 4096
skip_recovery_scan = true

[disk_pools.disks.config.file]
path = "/tmp/unbounded-storage-${LOCAL_NAME}.disk"
size = $DISK_SIZE

[[frontends]]
name = "fe"
source = "cache"

[frontends.config.http]
addr = "0.0.0.0:$OPT_FRONTEND_PORT"

[startup.fabric.binds.tcp]
# Bind the node's own routable IP, not 0.0.0.0. This must be the exact
# address peers use to reach this node (their [[neighborhoods.peers]] TCP addr
# points here); the libfabric tcp provider uses it both to bind and as its
# connection-manager identity and does not come up on an INADDR_ANY bind.
addr = "$LOCAL_IP:$OPT_PORT"

[startup.metrics]
# Prometheus text-format exporter on GET /metrics. Bind 0.0.0.0 so an
# in-cluster scraper (e.g. AKS managed Prometheus) can reach it across the
# network; an empty bind disables the exporter.
addr = "0.0.0.0:$OPT_METRICS_PORT"
EOF

# Close the output fd and report where the config landed (file mode only).
exec 3>&-
[[ "$OPT_OUTPUT" != "-" ]] && echo "wrote unbounded-storage config for node '$LOCAL_NAME' to $OPT_OUTPUT" >&2
