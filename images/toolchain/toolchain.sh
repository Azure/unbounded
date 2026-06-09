#!/usr/bin/env bash
#
# toolchain.sh
#
# Launch the project's toolchain container with the repository mounted at
# /project. Any arguments are forwarded to the container as the command; with
# no arguments the entrypoint drops into an interactive bash shell as the
# unprivileged "dev" user (mapped to the host UID/GID).
#
# Environment variables (all optional):
#   CONTAINER_ENGINE   Force a specific runtime. Default: podman if available,
#                      otherwise docker.
#   TOOLCHAIN_FLAVOR   Image flavor to build/run: "fedora" (default) or
#                      "ubuntu". Selects the matching Containerfile next to
#                      this script and determines the default image tag.
#   TOOLCHAIN_IMAGE    Image reference. Default: toolchain:${TOOLCHAIN_FLAVOR}
#                      (e.g. toolchain:fedora). Set this to override the
#                      derived tag (e.g. for pushing to a registry).
#   TOOLCHAIN_REBUILD  If non-empty, rebuild the image before running.
#   PROJECT_DIR        Host path mounted at /project. Default: the repository
#                      root inferred from this script's location.
#   AZURE_CONFIG_DIR   Host path forwarded to /host/.azure (read by the
#                      entrypoint, which copies it into the dev user's HOME so
#                      the Azure CLI default location picks it up). Default:
#                      $HOME/.azure. If the directory does not exist the
#                      mount is silently skipped.
#
# Pinned Go tool versions are NOT environment-driven; they are extracted from
# the repository Makefile (GOFUMPT_VERSION, GOLANGCI_LINT_VERSION,
# PROTOC_GEN_GO_VERSION, PROTOC_GEN_GO_GRPC_VERSION, CONTROLLER_GEN_VERSION)
# and passed to the image build as --build-arg flags. The Makefile is the
# single source of truth; update versions there.
#
# Examples:
#   ./images/toolchain/toolchain.sh                  # interactive shell
#   ./images/toolchain/toolchain.sh go test ./...    # run a one-off command
#   TOOLCHAIN_FLAVOR=ubuntu ./images/toolchain/toolchain.sh  # use ubuntu image
#   TOOLCHAIN_REBUILD=1 ./images/toolchain/toolchain.sh      # force rebuild
#   CONTAINER_ENGINE=docker ./images/toolchain/toolchain.sh

set -euo pipefail

log() {
    printf '[toolchain.sh] %s\n' "$*" >&2
}

die() {
    printf '[toolchain.sh] error: %s\n' "$*" >&2
    exit 1
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- Engine selection -------------------------------------------------------
engine="${CONTAINER_ENGINE:-}"
if [[ -z "${engine}" ]]; then
    if command -v podman >/dev/null 2>&1; then
        engine=podman
    elif command -v docker >/dev/null 2>&1; then
        engine=docker
    else
        die "neither podman nor docker found on PATH; install one or set CONTAINER_ENGINE"
    fi
fi

if ! command -v "${engine}" >/dev/null 2>&1; then
    die "CONTAINER_ENGINE=${engine} but it is not on PATH"
fi

# --- Paths ------------------------------------------------------------------
project_dir="${PROJECT_DIR:-$(cd "${script_dir}/../.." && pwd)}"
if [[ ! -d "${project_dir}" ]]; then
    die "project directory does not exist: ${project_dir}"
fi

# --- Flavor selection -------------------------------------------------------
# Pick the Containerfile that matches the requested flavor. Each flavor
# produces a distinctly tagged image so multiple flavors can coexist in the
# local image cache. TOOLCHAIN_IMAGE override (if set) still wins.
toolchain_flavor="${TOOLCHAIN_FLAVOR:-fedora}"
case "${toolchain_flavor}" in
    fedora)
        containerfile="${script_dir}/Containerfile"
        ;;
    ubuntu)
        containerfile="${script_dir}/Containerfile.ubuntu"
        ;;
    *)
        die "unknown TOOLCHAIN_FLAVOR=${toolchain_flavor} (expected: fedora, ubuntu)"
        ;;
esac

if [[ ! -f "${containerfile}" ]]; then
    die "Containerfile for flavor '${toolchain_flavor}' not found: ${containerfile}"
fi

toolchain_image="${TOOLCHAIN_IMAGE:-toolchain:${toolchain_flavor}}"

# --- Image build (lazy) -----------------------------------------------------
need_build=0
if [[ -n "${TOOLCHAIN_REBUILD:-}" ]]; then
    need_build=1
elif ! "${engine}" image inspect "${toolchain_image}" >/dev/null 2>&1; then
    need_build=1
fi

if (( need_build )); then
    # Extract a pinned tool version (matching `VAR ?= value` or `VAR = value`)
    # from the repository Makefile. Errors out if the variable is missing so
    # the image is never built with an unpinned `@latest`.
    extract_version() {
        local var="$1"
        local makefile="${project_dir}/Makefile"
        local val
        if [[ ! -f "${makefile}" ]]; then
            die "Makefile not found at ${makefile}"
        fi
        val="$(sed -nE "s/^[[:space:]]*${var}[[:space:]]*\\??=[[:space:]]*([^[:space:]#]+).*/\\1/p" "${makefile}" | head -n1)"
        if [[ -z "${val}" ]]; then
            die "could not extract ${var} from ${makefile}"
        fi
        printf '%s' "${val}"
    }

    gofumpt_version=$(extract_version GOFUMPT_VERSION)
    golangci_lint_version=$(extract_version GOLANGCI_LINT_VERSION)
    protoc_gen_go_version=$(extract_version PROTOC_GEN_GO_VERSION)
    protoc_gen_go_grpc_version=$(extract_version PROTOC_GEN_GO_GRPC_VERSION)
    controller_gen_version=$(extract_version CONTROLLER_GEN_VERSION)

    log "building ${toolchain_image} (${toolchain_flavor}) with ${engine} from ${script_dir}"
    log "  Containerfile=${containerfile}"
    log "  GOFUMPT_VERSION=${gofumpt_version}"
    log "  GOLANGCI_LINT_VERSION=${golangci_lint_version}"
    log "  PROTOC_GEN_GO_VERSION=${protoc_gen_go_version}"
    log "  PROTOC_GEN_GO_GRPC_VERSION=${protoc_gen_go_grpc_version}"
    log "  CONTROLLER_GEN_VERSION=${controller_gen_version}"

    "${engine}" build \
        --file "${containerfile}" \
        --build-arg "GOFUMPT_VERSION=${gofumpt_version}" \
        --build-arg "GOLANGCI_LINT_VERSION=${golangci_lint_version}" \
        --build-arg "PROTOC_GEN_GO_VERSION=${protoc_gen_go_version}" \
        --build-arg "PROTOC_GEN_GO_GRPC_VERSION=${protoc_gen_go_grpc_version}" \
        --build-arg "CONTROLLER_GEN_VERSION=${controller_gen_version}" \
        -t "${toolchain_image}" \
        "${script_dir}"
fi

# --- Azure CLI config passthrough ------------------------------------------
azure_src="${AZURE_CONFIG_DIR:-${HOME}/.azure}"
azure_flags=()
if [[ -d "${azure_src}" ]]; then
    azure_flags+=("--volume=${azure_src}:/host/.azure:z")
fi

# --- TTY flags --------------------------------------------------------------
tty_flags=()
if [[ -t 0 ]]; then
    tty_flags+=("-i")
fi
if [[ -t 1 ]]; then
    tty_flags+=("-t")
fi

# --- Engine-specific flags --------------------------------------------------
# The image bakes a "dev" user at uid:gid 1000:1000 and starts the entrypoint
# as that user (USER dev in the Containerfile). Under rootless podman, the
# user namespace would normally map container uid 1000 to a subordinate uid
# (e.g. 525287) that has no ownership of host-mounted files. We use
# --userns=keep-id:uid=1000,gid=1000 to instead map the host invoking user
# 1:1 onto container uid 1000. The result: inside the container `id` reports
# `uid=1000(dev)`, and writes to /project land on the host as the invoking
# user, with no need for entrypoint-side user/uid juggling.
#
# Docker (rootful) shares the host user namespace already; no extra flags
# are needed. The setup assumes the host user is uid 1000 in that case;
# non-1000 host uids on rootful docker would write files as host uid 1000,
# which is a known limitation.
engine_flags=()
if [[ "${engine}" == *podman* ]]; then
    engine_flags+=("--userns=keep-id:uid=1000,gid=1000")
fi

# --- Run --------------------------------------------------------------------
exec "${engine}" run \
    --rm \
    --init \
    ${tty_flags[@]+"${tty_flags[@]}"} \
    ${engine_flags[@]+"${engine_flags[@]}"} \
    "--volume=${project_dir}:/project:z" \
    --workdir=/project \
    ${azure_flags[@]+"${azure_flags[@]}"} \
    "${toolchain_image}" \
    "$@"
