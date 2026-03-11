#!/usr/bin/env bash
#
# Creates a MachineModel custom resource along with its SSH private key Secret.
#
# Usage:
#   hack/machina/create-machinemodel.sh <cluster-name> <model-name> -s <agent-install-script> -b <bootstrap-token-secret-name>
#
# Example:
#   hack/machina/create-machinemodel.sh dcdevy26w10r0 my-model \
#     -s ./scripts/install.sh \
#     -b bootstrap-token-abc123
#
# The script will:
#   1. Create a Secret (named <model-name>-ssh) in machina-system from
#      ~/.stargate/<cluster-name>/ssh/id_rsa
#   2. Apply a cluster-scoped MachineModel CR that references that Secret,
#      embeds the agent install script, and sets up the kubernetesProfile
#      with the given bootstrap token secret.
#
# Environment:
#   SSH_USER      SSH username (default: stargate)
#
set -euo pipefail

SSH_USER="${SSH_USER:-stargate}"
SECRET_NAMESPACE="machina-system"

# ── Inputs ───────────────────────────────────────────────────────────────────
CLUSTER_NAME="${1:-}"
MODEL_NAME="${2:-}"
shift 2 2>/dev/null || true

SCRIPT_PATH=""
BOOTSTRAP_TOKEN_SECRET=""

while getopts "s:b:" opt; do
  case "$opt" in
    s) SCRIPT_PATH="$OPTARG" ;;
    b) BOOTSTRAP_TOKEN_SECRET="$OPTARG" ;;
    *) ;;
  esac
done

if [[ -z "$CLUSTER_NAME" || -z "$MODEL_NAME" || -z "$SCRIPT_PATH" || -z "$BOOTSTRAP_TOKEN_SECRET" ]]; then
  echo "Usage: $(basename "$0") <cluster-name> <model-name> -s <agent-install-script> -b <bootstrap-token-secret-name>" >&2
  exit 1
fi

# ── Paths ────────────────────────────────────────────────────────────────────
SSH_KEY_PATH="$HOME/.stargate/${CLUSTER_NAME}/ssh/id_rsa"
if [[ ! -f "$SSH_KEY_PATH" ]]; then
  echo "Error: SSH private key not found at ${SSH_KEY_PATH}" >&2
  exit 1
fi

if [[ ! -f "$SCRIPT_PATH" ]]; then
  echo "Error: Agent install script not found at ${SCRIPT_PATH}" >&2
  exit 1
fi

SECRET_NAME="${MODEL_NAME}-ssh"

# ── Step 1: Create the SSH Secret ────────────────────────────────────────────
echo "Creating Secret ${SECRET_NAME} in namespace ${SECRET_NAMESPACE}..."
kubectl create secret generic "$SECRET_NAME" \
  --namespace "$SECRET_NAMESPACE" \
  --from-file=ssh-privatekey="$SSH_KEY_PATH" \
  --dry-run=client -o yaml | kubectl apply -f -

# ── Step 2: Apply the MachineModel ──────────────────────────────────────────
echo "Applying MachineModel ${MODEL_NAME}..."

# Indent the script content by 4 spaces for YAML block scalar embedding.
SCRIPT_CONTENT="$(sed 's/^/    /' "$SCRIPT_PATH")"

cat <<EOF | kubectl apply -f -
apiVersion: machina.unboundedkube.io/v1alpha2
kind: MachineModel
metadata:
  name: ${MODEL_NAME}
spec:
  sshUsername: ${SSH_USER}
  sshPrivateKeyRef:
    name: ${SECRET_NAME}
    key: ssh-privatekey
  agentInstallScript: |
${SCRIPT_CONTENT}
  kubernetesProfile:
    bootstrapTokenRef:
      name: ${BOOTSTRAP_TOKEN_SECRET}
EOF

echo ""
echo "Done."
echo "  Secret:       ${SECRET_NAMESPACE}/${SECRET_NAME}"
echo "  MachineModel: ${MODEL_NAME}"
