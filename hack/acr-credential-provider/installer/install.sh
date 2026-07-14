#!/usr/bin/env sh

set -eu

HOST_ROOT=${HOST_ROOT:-/host}
PROVIDER_SOURCE=${ACR_PROVIDER_SOURCE:-/unbounded/bin/acr-credential-provider}
PROVIDER_NAME=${ACR_PROVIDER_NAME:-acr-credential-provider}
HOST_BIN_DIR=${ACR_PROVIDER_BIN_DIR:-/var/lib/kubelet/credential-provider/bin}
HOST_CONFIG_PATH=${ACR_PROVIDER_CONFIG_PATH:-/var/lib/kubelet/credential-provider/config.yaml}
RESTART_JITTER_SECONDS=${ACR_PROVIDER_RESTART_JITTER_SECONDS:-180}
MANAGED_MARKER="# Managed by the Unbounded ACR credential provider installer."
MANAGED_END_MARKER="# End Unbounded ACR credential provider installer."

host_path() {
  printf '%s%s' "$HOST_ROOT" "$1"
}

atomic_write() {
  target=$1
  mode=$2
  tmp="${target}.tmp"
  cat > "$tmp"
  chmod "$mode" "$tmp"
  mv "$tmp" "$target"
}

install -d -m 0755 "$(host_path "$HOST_BIN_DIR")"
install -d -m 0755 "$(dirname "$(host_path "$HOST_CONFIG_PATH")")"

install -m 0755 "$PROVIDER_SOURCE" "$(host_path "$HOST_BIN_DIR")/${PROVIDER_NAME}"

if [ -e "$(host_path "$HOST_CONFIG_PATH")" ] && \
   ! grep -Fqx "$MANAGED_MARKER" "$(host_path "$HOST_CONFIG_PATH")"; then
  echo "refusing to overwrite unmanaged $HOST_CONFIG_PATH" >&2
  exit 1
fi

atomic_write "$(host_path "$HOST_CONFIG_PATH")" 0644 <<EOF
$MANAGED_MARKER
apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: ${PROVIDER_NAME}
    matchImages:
      - "*.azurecr.io"
    defaultCacheDuration: "1h"
    apiVersion: credentialprovider.kubelet.k8s.io/v1
EOF

extra_args="--image-credential-provider-config=${HOST_CONFIG_PATH} --image-credential-provider-bin-dir=${HOST_BIN_DIR}"
default_file="$(host_path /etc/default/kubelet)"
changed=0

if ! grep -q -- "--image-credential-provider-config=${HOST_CONFIG_PATH}" "$default_file" 2>/dev/null; then
  {
    printf '\n%s\n' "$MANAGED_MARKER"
    printf 'KUBELET_EXTRA_ARGS="${KUBELET_EXTRA_ARGS:-} %s"\n' "$extra_args"
    printf '%s\n' "$MANAGED_END_MARKER"
  } >> "$default_file"
  changed=1
fi

if [ "$changed" -eq 1 ]; then
  if [ "$RESTART_JITTER_SECONDS" -gt 0 ]; then
    random_value=$(od -An -N4 -tu4 /dev/urandom | tr -d ' ')
    sleep_seconds=$((random_value % (RESTART_JITTER_SECONDS + 1)))
    echo "sleeping ${sleep_seconds}s before restarting kubelet"
    sleep "$sleep_seconds"
  fi

  nsenter -t 1 -m -u -i -n -p -- systemctl daemon-reload
  nsenter -t 1 -m -u -i -n -p -- systemctl restart kubelet
fi

echo "installed ${PROVIDER_NAME} at ${HOST_BIN_DIR} with config ${HOST_CONFIG_PATH}"

exec sleep 2147483647
