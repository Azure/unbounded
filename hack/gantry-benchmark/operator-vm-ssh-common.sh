#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

operator_ssh_init() {
  : "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"

  local repository_root
  repository_root=$(git rev-parse --show-toplevel)
  OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"
  OPERATOR_SSH_HOST_ALIAS="${OPERATOR_SSH_HOST_ALIAS:-$OPERATOR_VM_NAME}"
  OPERATOR_SSH_CONFIG_PATH="${OPERATOR_SSH_CONFIG_PATH:-$repository_root/tmp/$AZURE_RESOURCE_GROUP/ssh-config}"

  [[ -f "$OPERATOR_SSH_CONFIG_PATH" ]] || {
    echo "missing operator SSH config $OPERATOR_SSH_CONFIG_PATH; run make -C hack/gantry-benchmark operator-vm-ssh-configure" >&2
    return 1
  }
}

operator_ssh() {
  ssh \
    -F "$OPERATOR_SSH_CONFIG_PATH" \
    -o BatchMode=yes \
    -o ConnectTimeout=20 \
    -o ServerAliveInterval=30 \
    -T \
    "$OPERATOR_SSH_HOST_ALIAS" \
    "$@"
}