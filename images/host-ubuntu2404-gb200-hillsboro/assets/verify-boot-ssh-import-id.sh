#!/bin/sh
set -eu

user=$(id -un)
identity=${1:?ssh-import-id identity is required}
test "$identity" = "gh:$user"

if [ "$user" = bcho ]; then
    echo "ssh-import-id must not be invoked for keyless bcho" >&2
    exit 1
fi

umask 077
mkdir -p "$HOME/.ssh"
public_key=$(python3 - <<'PY'
import base64
import struct


def field(value):
    return struct.pack(">I", len(value)) + value


blob = field(b"ssh-ed25519") + field(bytes(range(32)))
print("ssh-ed25519", base64.b64encode(blob).decode())
PY
)
printf '%s %s\n' "$public_key" "$identity" \
    > "$HOME/.ssh/authorized_keys"
