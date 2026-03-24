#!/usr/bin/env bash
#
# Creates an SSH private key Secret for use with Machine resources.
#
# Usage:
#   hack/machina/create-machinemodel.sh <cluster-name> <secret-name> -b <bootstrap-token-secret-name>
#
# Example:
#   hack/machina/create-machinemodel.sh dcdevy26w10r0 machina-ssh \
#     -b bootstrap-token-abc123
#
# The script will:
#   1. Create a Secret (named <secret-name>) in machina-system from
#      ~/.stargate/<cluster-name>/ssh/id_rsa
#   2. Print an example Machine CR that references the Secret.
#
# Environment:
#   SSH_USER      SSH username (default: azureuser)
#
set -euo pipefail

SSH_USER="${SSH_USER:-azureuser}"
SECRET_NAMESPACE="machina-system"

# ── Inputs ───────────────────────────────────────────────────────────────────
CLUSTER_NAME="${1:-}"
SECRET_NAME="${2:-}"
shift 2 2>/dev/null || true

BOOTSTRAP_TOKEN_SECRET=""

while getopts "b:" opt; do
  case "$opt" in
    b) BOOTSTRAP_TOKEN_SECRET="$OPTARG" ;;
    *) ;;
  esac
done

if [[ -z "$CLUSTER_NAME" || -z "$SECRET_NAME" ]]; then
  echo "Usage: $(basename "$0") <cluster-name> <secret-name> [-b <bootstrap-token-secret-name>]" >&2
  exit 1
fi

# ── Paths ────────────────────────────────────────────────────────────────────
SSH_KEY_PATH="$HOME/.stargate/${CLUSTER_NAME}/ssh/id_rsa"
if [[ ! -f "$SSH_KEY_PATH" ]]; then
  echo "Error: SSH private key not found at ${SSH_KEY_PATH}" >&2
  exit 1
fi

# ── Step 1: Create the SSH Secret ────────────────────────────────────────────
echo "Creating Secret ${SECRET_NAME} in namespace ${SECRET_NAMESPACE}..."
kubectl create secret generic "$SECRET_NAME" \
  --namespace "$SECRET_NAMESPACE" \
  --from-file=ssh-privatekey="$SSH_KEY_PATH" \
  --dry-run=client -o yaml | kubectl apply -f -

# ── Step 2: Print example Machine CR ────────────────────────────────────────
echo ""
echo "Done. Secret created: ${SECRET_NAMESPACE}/${SECRET_NAME}"
echo ""
echo "Example Machine CR referencing this secret:"
echo ""

BOOTSTRAP_BLOCK=""
if [[ -n "$BOOTSTRAP_TOKEN_SECRET" ]]; then
  BOOTSTRAP_BLOCK="  kubernetes:
    bootstrapTokenRef:
      name: ${BOOTSTRAP_TOKEN_SECRET}"
fi

cat <<EOF
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: <machine-name>
spec:
  ssh:
    host: "<ip>:<port>"
    username: ${SSH_USER}
    privateKeyRef:
      name: ${SECRET_NAME}
      key: ssh-privatekey
${BOOTSTRAP_BLOCK}
EOF
