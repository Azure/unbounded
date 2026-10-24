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
printf 'ssh-ed25519 deterministic-boot-qualification %s\n' "$identity" \
    > "$HOME/.ssh/authorized_keys"
