#!/usr/bin/env bash
set -euo pipefail

# The image's USER directive ensures this script runs as the unprivileged
# "dev" user (uid:gid 1000:1000) baked into the image. Under rootless podman,
# toolchain.sh passes --userns=keep-id:uid=1000,gid=1000 so that container
# uid 1000 maps 1:1 onto the host invoking user; that makes host-mounted
# /project files readable/writable as the same user inside and outside the
# container. Under rootful docker, container uid 1000 == host uid 1000
# directly (no namespace remapping).
#
# This script no longer mutates /etc/passwd or /etc/sudoers.d at runtime;
# those are set up at image build time (see Containerfile).

export HOME=/home/dev
export GOPATH="$HOME/go"
export GOBIN="$HOME/go/bin"
mkdir -p "$GOBIN"
export PATH="$GOBIN:$PATH"

if [[ -d /host/.azure ]]; then
    # Copy rather than symlink so host and container remain decoupled.
    # The container's HOME is dev-owned, so this works without privileges.
    cp -R /host/.azure "$HOME/.azure"
fi

if [ $# -ne 0 ]; then
    exec "$@"
else
    exec /bin/bash
fi
