#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# migrate-namespace.sh -- migrate an existing unbounded install from the legacy
# split namespaces (machina/metalman in `unbounded-kube`, net in `unbounded-net`)
# onto the consolidated `unbounded-system` namespace.
#
# Kubernetes namespaces cannot be renamed in place, so this performs a one-time,
# sequenced cutover: it copies operator-owned state (Secrets, the machina-config
# ConfigMap, metalman PXE Deployments) into the target namespace, redeploys the
# components there from the released manifests, rewrites the cluster-scoped CR
# secret references that enforce a namespace, and finally decommissions the old
# namespaces.
#
# NOTE: For clusters managed by unbounded-operator, prefer the operator-driven
# migration (start the operator with --reap-legacy-resources); it performs the
# same copy/rewrite/reap steps continuously and is the supported path. This
# script remains for air-gapped or non-operator installs. Either way, the legacy
# Namespace objects are left for a human to delete once empty.
#
# Scope: machina, metalman, and net only. Cluster-scoped custom resources
# (Site, Machine, GatewayPool, ...) and CRDs are NOT namespaced and are left
# untouched. Gantry (`gantry-system`) and inventory (`machina-system`) are out
# of scope; the script warns if it sees them but does not modify them.
#
# The script is idempotent: a run that fails partway can be re-run to completion
# and the old namespaces are deleted only after the new components are healthy,
# so a failure before that point leaves a recoverable cluster.
#
# There IS a brief net data-plane interruption during the cutover (the old
# net-node DaemonSet is removed before the new one is Ready). This is inherent:
# old and new net cannot run in parallel.
#
# Usage:
#   hack/scripts/migrate-namespace.sh (--release TAG | --manifests-dir DIR) [options]
#
# The new components are deployed from the released, unbounded-system rendered
# manifests. Provide them either by release tag (downloaded from GitHub) or as a
# local directory:
#
#   --release TAG             Download unbounded-manifests-TAG.tar.gz from the
#                             GitHub release TAG (e.g. v0.2.0) and use it.
#   --repo OWNER/REPO         GitHub repository to download from
#                             (default: Azure/unbounded).
#   --manifests-dir DIR       Use already-extracted manifests instead of
#                             downloading. Must contain `machina/` and `net/`
#                             subdirectories (the layout inside the
#                             unbounded-manifests-TAG.tar.gz release asset).
#
# Options:
#   --target-namespace NS     Consolidated namespace (default: unbounded-system).
#   --machina-namespace NS    Legacy machina/metalman namespace (default: unbounded-kube).
#   --net-namespace NS        Legacy net namespace (default: unbounded-net).
#   --context CTX             kubectl context to target (default: current).
#   --keep-old-namespaces     Do not delete the old namespaces at the end.
#   --dry-run                 Print the plan; make no changes.
#   --yes                     Do not prompt for confirmation.
#   -h | --help               Show this help.
#
# Examples:
#   hack/scripts/migrate-namespace.sh --release v0.2.0 --dry-run
#   hack/scripts/migrate-namespace.sh --release v0.2.0
#   hack/scripts/migrate-namespace.sh --manifests-dir ./unbounded-manifests-v0.2.0
#
# Exit codes:
#   0 success (or already migrated)
#   1 usage / validation / migration error

set -euo pipefail

# ── configuration ──────────────────────────────────────────────────────────
MANIFESTS_DIR=""
RELEASE_TAG=""
REPO="Azure/unbounded"
# Base URL for release downloads. Overridable only so the smoke test can point
# at a local server; not a documented user-facing option.
RELEASE_BASE_URL="${UNBOUNDED_RELEASE_BASE_URL:-https://github.com}"
TARGET_NS="unbounded-system"
OLD_MACHINA_NS="unbounded-kube"
OLD_NET_NS="unbounded-net"
CONTEXT=""
KEEP_OLD="false"
DRY_RUN="false"
ASSUME_YES="false"

# Temp directories to clean up on exit (download + snapshot working dirs).
CLEANUP_DIRS=()

# Regenerable resources we deliberately do NOT copy (the net controller
# recreates its serving cert on startup).
SKIP_SECRET_NAMES=("unbounded-net-serving-cert")
# Secret types that are auto-managed and must never be copied.
SKIP_SECRET_TYPES=("kubernetes.io/service-account-token" "helm.sh/release.v1")

ROLLOUT_TIMEOUT="5m"
NS_DELETE_TIMEOUT="120s"

WORKDIR=""

# ── helpers ────────────────────────────────────────────────────────────────
log()  { printf '>> %s\n' "$*" >&2; }
warn() { printf '!! %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

cleanup() {
	local dir
	for dir in "${CLEANUP_DIRS[@]}"; do
		[[ -n "${dir}" && -d "${dir}" ]] && rm -rf "${dir}"
	done
}
trap cleanup EXIT

usage() {
	sed -n '/^# Usage:/,/^# *1 usage/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

require_value() { # require_value <flag> <value>
	if [[ -z "${2:-}" || "${2:0:2}" == "--" ]]; then
		die "flag $1 requires a value"
	fi
}

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "$1 not found on PATH. $2"; }

# kubectl wrapper honoring --context.
kc() {
	if [[ -n "${CONTEXT}" ]]; then
		kubectl --context "${CONTEXT}" "$@"
	else
		kubectl "$@"
	fi
}

# Mutating wrapper: prints (and skips) the command under --dry-run.
kc_mutate() {
	if [[ "${DRY_RUN}" == "true" ]]; then
		printf '   [dry-run] kubectl %s\n' "$*" >&2
		return 0
	fi
	kc "$@"
}

# Apply a snapshot file (no-op if it does not exist, e.g. nothing to migrate).
apply_file() { # apply_file <path>
	local path="$1"
	[[ -f "${path}" ]] || return 0
	if [[ "${DRY_RUN}" == "true" ]]; then
		printf '   [dry-run] kubectl apply -f %s\n' "${path}" >&2
		return 0
	fi
	kc apply -f "${path}"
}

ns_exists() { kc get namespace "$1" >/dev/null 2>&1; }

crd_exists() { kc get crd "$1" >/dev/null 2>&1; }

# Sanitize a resource JSON for re-creation in another namespace: drop
# server-managed metadata and status, and set the namespace.
sanitize_json() { # sanitize_json <target-namespace>
	jq --arg ns "$1" '
		del(
			.metadata.resourceVersion,
			.metadata.uid,
			.metadata.creationTimestamp,
			.metadata.generation,
			.metadata.managedFields,
			.metadata.ownerReferences,
			.metadata.selfLink,
			.metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"],
			.status
		)
		| .metadata.namespace = $ns
	'
}

# ── argument parsing ───────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
	case "$1" in
		--release)            require_value "$1" "${2:-}"; RELEASE_TAG="$2"; shift 2 ;;
		--repo)               require_value "$1" "${2:-}"; REPO="$2"; shift 2 ;;
		--manifests-dir)      require_value "$1" "${2:-}"; MANIFESTS_DIR="$2"; shift 2 ;;
		--target-namespace)   require_value "$1" "${2:-}"; TARGET_NS="$2"; shift 2 ;;
		--machina-namespace)  require_value "$1" "${2:-}"; OLD_MACHINA_NS="$2"; shift 2 ;;
		--net-namespace)      require_value "$1" "${2:-}"; OLD_NET_NS="$2"; shift 2 ;;
		--context)            require_value "$1" "${2:-}"; CONTEXT="$2"; shift 2 ;;
		--keep-old-namespaces) KEEP_OLD="true"; shift ;;
		--dry-run)            DRY_RUN="true"; shift ;;
		--yes)                ASSUME_YES="true"; shift ;;
		-h|--help)            usage; exit 0 ;;
		*)                    die "unknown argument: $1 (try --help)" ;;
	esac
done

require_cmd kubectl "Install from https://kubernetes.io/docs/tasks/tools/"
require_cmd jq      "Install from https://jqlang.github.io/jq/"

if [[ -n "${RELEASE_TAG}" && -n "${MANIFESTS_DIR}" ]]; then
	die "pass only one of --release or --manifests-dir"
fi
if [[ -z "${RELEASE_TAG}" && -z "${MANIFESTS_DIR}" ]]; then
	die "one of --release TAG or --manifests-dir DIR is required (try --help)"
fi

# Download and extract the release manifests tarball when --release is given,
# pointing MANIFESTS_DIR at the extracted tree.
resolve_manifests() {
	[[ -n "${RELEASE_TAG}" ]] || return 0
	require_cmd curl "Install from https://curl.se/"
	require_cmd tar  "Install tar from your distribution"

	local asset="unbounded-manifests-${RELEASE_TAG}.tar.gz"
	local url="${RELEASE_BASE_URL}/${REPO}/releases/download/${RELEASE_TAG}/${asset}"
	local dl
	dl="$(mktemp -d)"
	CLEANUP_DIRS+=("${dl}")

	log "Downloading ${url}"
	if ! curl -fsSL "${url}" -o "${dl}/${asset}"; then
		die "failed to download ${asset} from ${REPO} release ${RELEASE_TAG} (${url})"
	fi
	log "Extracting ${asset}"
	tar -xzf "${dl}/${asset}" -C "${dl}" || die "failed to extract ${asset}"

	MANIFESTS_DIR="${dl}/unbounded-manifests-${RELEASE_TAG}"
}

validate_manifests_dir() {
	[[ -d "${MANIFESTS_DIR}/machina" ]] || die "missing ${MANIFESTS_DIR}/machina; manifests must contain machina/ and net/"
	[[ -d "${MANIFESTS_DIR}/net" ]]     || die "missing ${MANIFESTS_DIR}/net; manifests must contain machina/ and net/"
}

# ── phases ─────────────────────────────────────────────────────────────────

preflight() {
	log "Preflight: verifying cluster access and discovering legacy state"
	kc version -o json >/dev/null 2>&1 || die "cannot reach the cluster (check --context / kubeconfig)"

	# Out-of-scope namespaces we will not touch but want the operator to know about.
	for ns in gantry-system machina-system; do
		if ns_exists "${ns}"; then
			warn "namespace '${ns}' exists but is out of scope (gantry/inventory); leaving it untouched"
		fi
	done

	local found="false"
	for ns in "${OLD_MACHINA_NS}" "${OLD_NET_NS}"; do
		if ns_exists "${ns}"; then
			found="true"
		else
			warn "legacy namespace '${ns}' not found; skipping its resources"
		fi
	done

	if [[ "${found}" == "false" ]]; then
		if ns_exists "${TARGET_NS}"; then
			log "No legacy namespaces present and '${TARGET_NS}' exists; nothing to migrate."
			exit 0
		fi
		die "neither legacy namespace (${OLD_MACHINA_NS}, ${OLD_NET_NS}) nor target (${TARGET_NS}) found; is this an unbounded cluster?"
	fi

	WORKDIR="$(mktemp -d)"
	CLEANUP_DIRS+=("${WORKDIR}")
	mkdir -p "${WORKDIR}/secrets" "${WORKDIR}/pxe"

	snapshot_secrets
	snapshot_machina_config
	snapshot_pxe_deployments
	discover_cr_refs

	log "Plan:"
	log "  target namespace      : ${TARGET_NS}"
	log "  legacy machina/metalman: ${OLD_MACHINA_NS}"
	log "  legacy net            : ${OLD_NET_NS}"
	log "  secrets to copy       : $(find "${WORKDIR}/secrets" -name '*.json' | wc -l | tr -d ' ')"
	log "  machina-config        : $([[ -f "${WORKDIR}/machina-config.json" ]] && echo yes || echo none)"
	log "  metalman PXE deploys  : $(find "${WORKDIR}/pxe" -name '*.json' | wc -l | tr -d ' ')"
	log "  CR refs to rewrite    : $(wc -l <"${WORKDIR}/cr-refs.tsv" | tr -d ' ')"
	log "  delete old namespaces : $([[ "${KEEP_OLD}" == "true" ]] && echo no || echo yes)"

	if [[ "${DRY_RUN}" != "true" && "${ASSUME_YES}" != "true" ]]; then
		printf 'Proceed with migration? This briefly interrupts the net data plane. [y/N] ' >&2
		local reply
		read -r reply
		[[ "${reply}" == "y" || "${reply}" == "Y" ]] || die "aborted by user"
	fi
}

# Snapshot operator-owned Secrets from both legacy namespaces (sanitized,
# namespace rewritten), skipping auto-managed and regenerable ones.
snapshot_secrets() {
	local skip_types skip_names
	skip_types="$(printf '%s\n' "${SKIP_SECRET_TYPES[@]}" | jq -R . | jq -s .)"
	skip_names="$(printf '%s\n' "${SKIP_SECRET_NAMES[@]}" | jq -R . | jq -s .)"

	local ns
	for ns in "${OLD_MACHINA_NS}" "${OLD_NET_NS}"; do
		ns_exists "${ns}" || continue
		local names
		names="$(kc get secrets -n "${ns}" -o json |
			jq -r --argjson st "${skip_types}" --argjson sn "${skip_names}" \
				'.items[] | select((.type as $t | $st | index($t)) | not)
				           | select(.metadata.name as $n | $sn | index($n) | not)
				           | .metadata.name')"
		local name
		for name in ${names}; do
			kc get secret "${name}" -n "${ns}" -o json |
				sanitize_json "${TARGET_NS}" >"${WORKDIR}/secrets/${ns}__${name}.json"
		done
	done
}

snapshot_machina_config() {
	ns_exists "${OLD_MACHINA_NS}" || return 0
	if kc get configmap machina-config -n "${OLD_MACHINA_NS}" >/dev/null 2>&1; then
		kc get configmap machina-config -n "${OLD_MACHINA_NS}" -o json |
			sanitize_json "${TARGET_NS}" >"${WORKDIR}/machina-config.json"
	fi
}

# Metalman PXE Deployments are operator-created (per-site `deploy-pxe`) with
# site-specific args, so we copy the live objects rather than re-deriving them.
snapshot_pxe_deployments() {
	ns_exists "${OLD_MACHINA_NS}" || return 0
	local names
	names="$(kc get deploy -n "${OLD_MACHINA_NS}" -l app=unbounded-pxe \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
	local name
	for name in ${names}; do
		kc get deploy "${name}" -n "${OLD_MACHINA_NS}" -o json |
			sanitize_json "${TARGET_NS}" |
			jq 'del(.spec.template.metadata.creationTimestamp)' \
				>"${WORKDIR}/pxe/${name}.json"
	done
}

# Find cluster-scoped CRs whose secret reference enforces a namespace and still
# points at a legacy namespace. Records "<kind>\t<name>\t<jsonpath-prefix>".
discover_cr_refs() {
	: >"${WORKDIR}/cr-refs.tsv"
	local olds="${OLD_MACHINA_NS} ${OLD_NET_NS}"

	if crd_exists machineoperationcredentials.unbounded-cloud.io; then
		while IFS=$'\t' read -r name ns; do
			[[ -n "${name}" ]] || continue
			if [[ " ${olds} " == *" ${ns} "* ]]; then
				printf 'machineoperationcredential\t%s\tspec.auth.secretRef\n' "${name}" >>"${WORKDIR}/cr-refs.tsv"
			fi
		done < <(kc get machineoperationcredential -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.auth.secretRef.namespace}{"\n"}{end}' 2>/dev/null || true)
	fi

	if crd_exists machines.unbounded-cloud.io; then
		while IFS=$'\t' read -r name ns; do
			[[ -n "${name}" ]] || continue
			if [[ " ${olds} " == *" ${ns} "* ]]; then
				printf 'machine\t%s\tspec.pxe.redfish.passwordRef\n' "${name}" >>"${WORKDIR}/cr-refs.tsv"
			fi
		done < <(kc get machine -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.pxe.redfish.passwordRef.namespace}{"\n"}{end}' 2>/dev/null || true)
	fi
}

ensure_target_namespace() {
	if ns_exists "${TARGET_NS}"; then
		return 0
	fi
	log "Creating target namespace '${TARGET_NS}'"
	kc_mutate create namespace "${TARGET_NS}"
}

copy_secrets() {
	local count
	count="$(find "${WORKDIR}/secrets" -name '*.json' | wc -l | tr -d ' ')"
	log "Copying ${count} operator Secret(s) into '${TARGET_NS}'"
	local f
	for f in "${WORKDIR}"/secrets/*.json; do
		[[ -e "${f}" ]] || continue
		apply_file "${f}"
	done
}

quiesce_old() {
	log "Quiescing legacy components (scaling controllers to 0, removing old net-node)"
	# Stop reconciliation / leadership in the old namespaces so they cannot
	# fight the new install. Deleting the old net-node DaemonSet starts the
	# brief data-plane gap.
	if ns_exists "${OLD_MACHINA_NS}"; then
		kc_mutate -n "${OLD_MACHINA_NS}" scale deploy/machina-controller --replicas=0 2>/dev/null || true
		local d
		for d in $(kc get deploy -n "${OLD_MACHINA_NS}" -l app=unbounded-pxe -o name 2>/dev/null || true); do
			kc_mutate -n "${OLD_MACHINA_NS}" scale "${d}" --replicas=0 || true
		done
		# metalman controller deployments (non-PXE), if any were named uniformly.
		kc_mutate -n "${OLD_MACHINA_NS}" scale deploy/metalman-controller --replicas=0 2>/dev/null || true
	fi
	if ns_exists "${OLD_NET_NS}"; then
		kc_mutate -n "${OLD_NET_NS}" scale deploy/unbounded-net-controller --replicas=0 2>/dev/null || true
		kc_mutate -n "${OLD_NET_NS}" delete daemonset/unbounded-net-node --ignore-not-found 2>/dev/null || true
	fi
}

deploy_new() {
	log "Deploying machina + net into '${TARGET_NS}' from ${MANIFESTS_DIR}"
	# Server-side apply mirrors the release-upgrade path. The shipped manifests
	# already carry correct unbounded-system references (RBAC subjects, webhook
	# clientConfig, VAP CEL identities, leader-election namespaces).
	if [[ -d "${MANIFESTS_DIR}/net/crd" ]]; then
		kc_mutate apply --server-side --force-conflicts -f "${MANIFESTS_DIR}/net/crd/"
	fi
	if [[ -d "${MANIFESTS_DIR}/machina/crd" ]]; then
		kc_mutate apply --server-side --force-conflicts -f "${MANIFESTS_DIR}/machina/crd/"
	fi
	kc_mutate apply --server-side --force-conflicts -R -f "${MANIFESTS_DIR}/net/"
	kc_mutate apply --server-side --force-conflicts -R -f "${MANIFESTS_DIR}/machina/"

	# The bundle ships a static machina-config; re-apply the operator's live one
	# last so the per-cluster apiServerEndpoint is preserved.
	if [[ -f "${WORKDIR}/machina-config.json" ]]; then
		log "Restoring operator machina-config (preserving apiServerEndpoint)"
		apply_file "${WORKDIR}/machina-config.json"
	fi
}

deploy_pxe() {
	local count
	count="$(find "${WORKDIR}/pxe" -name '*.json' | wc -l | tr -d ' ')"
	[[ "${count}" -gt 0 ]] || return 0
	log "Recreating ${count} metalman PXE Deployment(s) in '${TARGET_NS}'"
	local f
	for f in "${WORKDIR}"/pxe/*.json; do
		[[ -e "${f}" ]] || continue
		apply_file "${f}"
	done
}

wait_healthy() {
	if [[ "${DRY_RUN}" == "true" ]]; then
		log "[dry-run] would wait for rollouts in '${TARGET_NS}'"
		return 0
	fi
	log "Waiting for new components to become healthy in '${TARGET_NS}'"
	kc -n "${TARGET_NS}" rollout status deploy/machina-controller --timeout="${ROLLOUT_TIMEOUT}"
	kc -n "${TARGET_NS}" rollout status deploy/unbounded-net-controller --timeout="${ROLLOUT_TIMEOUT}"
	kc -n "${TARGET_NS}" rollout status ds/unbounded-net-node --timeout="${ROLLOUT_TIMEOUT}"
	local f name
	for f in "${WORKDIR}"/pxe/*.json; do
		[[ -e "${f}" ]] || continue
		name="$(jq -r '.metadata.name' "${f}")"
		kc -n "${TARGET_NS}" rollout status "deploy/${name}" --timeout="${ROLLOUT_TIMEOUT}"
	done
}

rewrite_cr_refs() {
	[[ -s "${WORKDIR}/cr-refs.tsv" ]] || return 0
	log "Rewriting cluster-scoped CR secret references to '${TARGET_NS}'"
	local kind name prefix patch
	while IFS=$'\t' read -r kind name prefix; do
		[[ -n "${kind}" ]] || continue
		case "${prefix}" in
			spec.auth.secretRef)          patch='{"spec":{"auth":{"secretRef":{"namespace":"'"${TARGET_NS}"'"}}}}' ;;
			spec.pxe.redfish.passwordRef) patch='{"spec":{"pxe":{"redfish":{"passwordRef":{"namespace":"'"${TARGET_NS}"'"}}}}}' ;;
			*) warn "unknown CR ref prefix '${prefix}' for ${kind}/${name}; skipping"; continue ;;
		esac
		kc_mutate patch "${kind}" "${name}" --type=merge -p "${patch}"
	done <"${WORKDIR}/cr-refs.tsv"
}

decommission_old() {
	if [[ "${KEEP_OLD}" == "true" ]]; then
		log "--keep-old-namespaces set; leaving ${OLD_MACHINA_NS} and ${OLD_NET_NS} in place"
		return 0
	fi
	log "Decommissioning legacy namespaces (${OLD_MACHINA_NS}, ${OLD_NET_NS})"
	local ns
	for ns in "${OLD_MACHINA_NS}" "${OLD_NET_NS}"; do
		ns_exists "${ns}" || continue
		kc_mutate delete namespace "${ns}" --ignore-not-found --wait=true --timeout="${NS_DELETE_TIMEOUT}"
	done
}

summary() {
	log "Migration complete."
	if [[ "${DRY_RUN}" == "true" ]]; then
		log "(dry-run: no changes were made)"
		return 0
	fi
	kc -n "${TARGET_NS}" get deploy,ds,svc 2>/dev/null >&2 || true
}

main() {
	resolve_manifests
	validate_manifests_dir
	preflight
	ensure_target_namespace
	copy_secrets
	quiesce_old
	deploy_new
	deploy_pxe
	wait_healthy
	rewrite_cr_refs
	decommission_old
	summary
}

main
