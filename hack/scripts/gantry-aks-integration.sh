#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMMAND=run
SUBSCRIPTION="${SUBSCRIPTION:-}"
RESOURCE_GROUP="${RESOURCE_GROUP:-}"
LOCATION="${LOCATION:-canadacentral}"
NODE_COUNT="${NODE_COUNT:-10}"
NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D4s_v5}"
ACR_MIRROR_DIR="${ACR_MIRROR_DIR:-$REPO_ROOT/../acr-mirror}"
IMAGE_MAP="${IMAGE_MAP:-}"
RUN_ID="${RUN_ID:-$(date -u +%Y%m%d%H%M%S)}"
POD_TIMEOUT="${POD_TIMEOUT:-1200s}"
BATCH_DELAY="${BATCH_DELAY:-600}"
STATE_DIR="${STATE_DIR:-}"
HELM="${HELM:-$REPO_ROOT/bin/helm}"
GANTRY_NAMESPACE=unbounded-system
TEST_NAMESPACE=gantry-as-e2e
OWNER=unbounded-gantry-as-e2e
STAGE=initializing
RESULT=running
PUBLISH_READY=false
LOCK_HELD=false

usage() {
	cat <<'EOF'
Usage: hack/scripts/gantry-aks-integration.sh [run|deploy|test|publish|cleanup] OPTIONS

One entrypoint for an AKS cluster, Gantry artifact streaming, the acr-mirror
workload corpus, and an Azure Storage results dashboard. Resources are retained.
Repeated runs reuse owned infrastructure and immutable image tags.

Required:
  --subscription ID       Explicit Azure subscription.
  --resource-group NAME   Dedicated cluster resource group.

Options:
  --location REGION      Default: canadacentral.
  --nodes COUNT          Total nodes, including one system node. Default: 10.
  --vm-size SKU          Default: Standard_D4s_v5.
  --acr-mirror-dir PATH  Existing acr-mirror checkout. Default: ../acr-mirror.
  --image-map PATH       JSON object mapping original fixture refs to accessible
                         source refs. Images are imported into the owned ACR.
  --run-id ID            Stable ID to resume a run; default: UTC timestamp.
  --state-dir PATH       Default: tmp/gantry-aks-integration/<resource-group>.
  --pod-timeout VALUE    kubectl wait timeout. Default: 1200s.
  --batch-delay SECONDS  Original suite's delay between batches. Default: 600.
  --help                 Show this help.

run deploys, imports/converts fixtures, runs the existing workloads, and publishes.
deploy installs Gantry without importing or executing fixtures.
test uses an already deployed cluster and then publishes results.
publish republishes an existing --run-id.
cleanup deletes only the owned cluster resource group, not the results group.

Azure CLI, kubectl, Python 3, ssh-keygen, and Helm are required. Builds use ACR
Tasks, so Docker and Go do not need to be installed locally. Source registry
permissions are required even when the destination is your own ACR. No fixture
is silently replaced or skipped.
EOF
}

log() { printf '\n== %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

require_value() {
	[[ -n "${2:-}" && "$2" != --* ]] || die "$1 requires a value"
}

parse_args() {
	if [[ $# -gt 0 && "$1" != -* ]]; then
		COMMAND="$1"
		shift
	fi
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--subscription) require_value "$1" "${2:-}"; SUBSCRIPTION="$2"; shift 2 ;;
		--resource-group) require_value "$1" "${2:-}"; RESOURCE_GROUP="$2"; shift 2 ;;
		--location) require_value "$1" "${2:-}"; LOCATION="$2"; shift 2 ;;
		--nodes) require_value "$1" "${2:-}"; NODE_COUNT="$2"; shift 2 ;;
		--vm-size) require_value "$1" "${2:-}"; NODE_VM_SIZE="$2"; shift 2 ;;
		--acr-mirror-dir) require_value "$1" "${2:-}"; ACR_MIRROR_DIR="$2"; shift 2 ;;
		--image-map) require_value "$1" "${2:-}"; IMAGE_MAP="$2"; shift 2 ;;
		--run-id) require_value "$1" "${2:-}"; RUN_ID="$2"; shift 2 ;;
		--state-dir) require_value "$1" "${2:-}"; STATE_DIR="$2"; shift 2 ;;
		--pod-timeout) require_value "$1" "${2:-}"; POD_TIMEOUT="$2"; shift 2 ;;
		--batch-delay) require_value "$1" "${2:-}"; BATCH_DELAY="$2"; shift 2 ;;
		--help|-h) usage; exit 0 ;;
		*) die "unknown argument: $1" ;;
		esac
	done
	case "$COMMAND" in run|deploy|test|publish|cleanup) ;; *) die "unknown command: $COMMAND" ;; esac
	[[ "$SUBSCRIPTION" =~ ^[a-fA-F0-9-]{36}$ ]] || die "--subscription must be a subscription UUID"
	[[ "$RESOURCE_GROUP" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$ ]] || die "invalid --resource-group"
	[[ "$RUN_ID" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "invalid --run-id"
	[[ "$NODE_COUNT" =~ ^[0-9]+$ && "$NODE_COUNT" -ge 2 ]] || die "--nodes must be at least 2"
	[[ "$BATCH_DELAY" =~ ^[0-9]+$ ]] || die "--batch-delay must be nonnegative"
	[[ "$POD_TIMEOUT" =~ ^[1-9][0-9]*[smh]$ ]] || die "--pod-timeout must have an s, m, or h suffix"
}

az_cmd() {
	az "$@" --subscription "$SUBSCRIPTION" --only-show-errors
}

kube() {
	kubectl --kubeconfig "$STATE_DIR/kubeconfig" "$@"
}

json_field() {
	python3 -c '
import json, sys
value = json.load(sys.stdin)
for part in sys.argv[1].split("."):
    value = value[int(part)] if isinstance(value, list) else value[part]
if isinstance(value, (dict, list)):
    print(json.dumps(value))
elif isinstance(value, bool):
    print(str(value).lower())
elif value is not None:
    print(value)
' "$1"
}

initialize() {
	# This retained artifact-streaming runner targets a distinct chart/image
	# variant. Fail before Azure writes rather than deploying unsupported values.
	if [[ "$COMMAND" == run || "$COMMAND" == deploy ]]; then
		[[ -f "$REPO_ROOT/images/gantry-node-config/Containerfile" ]] || die "artifact-streaming Gantry image/chart support is absent from this checkout; this runner cannot deploy the standard Gantry chart"
	fi
	local tool
	for tool in az kubectl python3 ssh-keygen git; do
		command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
	done
	[[ -x "$HELM" ]] || die "missing Helm: run make install-helm"
	[[ -f "$ACR_MIRROR_DIR/tools/aks/create-yaml-pod-files-amd64.sh" ]] || die "invalid acr-mirror checkout: $ACR_MIRROR_DIR"
	[[ -z "$IMAGE_MAP" || -f "$IMAGE_MAP" ]] || die "image map does not exist: $IMAGE_MAP"
	ACR_MIRROR_DIR="$(cd "$ACR_MIRROR_DIR" && pwd)"
	local suffix
	suffix="$(printf '%s' "$SUBSCRIPTION/$RESOURCE_GROUP" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest()[:16])')"
	ACR="gantryas${suffix}"
	STORAGE_ACCOUNT="gantryas${suffix}"
	RESULTS_GROUP="${RESOURCE_GROUP}-results"
	CLUSTER=gantry-as-e2e
	WORKERS=$((NODE_COUNT - 1))
	MONIKER="g$(printf '%s' "$RUN_ID" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest()[:10])')"
	STATE_DIR="${STATE_DIR:-$REPO_ROOT/tmp/gantry-aks-integration/$RESOURCE_GROUP}"
	umask 077
	mkdir -p "$STATE_DIR"
	STATE_DIR="$(cd "$STATE_DIR" && pwd)"
	RUN_DIR="$STATE_DIR/runs/$RUN_ID"
	mkdir -p "$RUN_DIR/publish"
	if ! mkdir "$STATE_DIR/lock" 2>/dev/null; then
		die "deployment is locked at $STATE_DIR/lock; verify the recorded PID before removing a stale lock"
	fi
	LOCK_HELD=true
	printf '%s\n' "$$" > "$STATE_DIR/lock/pid"
	trap finish EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	az_cmd account show --output none
	export AZURE_CORE_COLLECT_TELEMETRY=no
	export AZURE_EXTENSION_DIR="$STATE_DIR/extensions"
	python3 - "$STATE_DIR/target.json" "$SUBSCRIPTION" "$RESOURCE_GROUP" <<'PY'
import json, os, sys
path, subscription, group = sys.argv[1:]
expected = {"subscription": subscription, "resourceGroup": group}
if os.path.exists(path):
    with open(path) as source:
        if json.load(source) != expected:
            raise SystemExit("state directory belongs to another deployment")
else:
    with open(path, "w") as target:
        json.dump(expected, target)
PY
}

ensure_group() {
	local group="$1" exists owner
	exists="$(az_cmd group exists --name "$group" --output tsv)"
	if [[ "$exists" == true ]]; then
		owner="$(az_cmd group show --name "$group" --query tags.gantryIntegration --output tsv)"
		[[ "$owner" == "$OWNER" ]] || die "refusing to adopt unowned resource group $group"
	else
		az_cmd group create --name "$group" --location "$LOCATION" \
			--tags "gantryIntegration=$OWNER" --output none
	fi
}

ensure_registry() {
	local existing
	existing="$(az_cmd acr list --resource-group "$RESOURCE_GROUP" --query "[?name=='$ACR'].id | [0]" --output tsv)"
	if [[ -z "$existing" ]]; then
		az_cmd acr create --name "$ACR" --resource-group "$RESOURCE_GROUP" \
			--location "$LOCATION" --sku Premium --admin-enabled false --output none
	fi
	[[ "$(az_cmd acr show --name "$ACR" --query sku.name --output tsv)" == Premium ]] || die "owned ACR must use the Premium SKU"
	REGISTRY="$(az_cmd acr show --name "$ACR" --query loginServer --output tsv)"
}

source_tag() {
	python3 - "$REPO_ROOT" <<'PY'
import hashlib, pathlib, subprocess, sys
root = pathlib.Path(sys.argv[1])
paths = subprocess.check_output(
    ["git", "-C", str(root), "ls-files", "-z", "--cached", "--others",
     "--exclude-standard", "--", "*.go", "go.mod", "go.sum", "Makefile",
     "images/gantry/Containerfile", "images/gantry-node-config/*"])
digest = hashlib.sha256()
for name in sorted(set(paths.split(b"\0")) - {b""}):
    path = root / name.decode()
    if path.is_file():
        digest.update(name + b"\0" + path.read_bytes())
print("e2e-" + digest.hexdigest()[:20])
PY
}

image_exists() {
	local repository="$1" tag="$2" found
	found="$(az_cmd acr repository list --name "$ACR" --query "[?@=='$repository'] | length(@)" --output tsv)"
	[[ "$found" != 0 ]] || return 1
	found="$(az_cmd acr repository show-tags --name "$ACR" --repository "$repository" --query "[?@=='$tag'] | length(@)" --output tsv)"
	[[ "$found" != 0 ]]
}

ensure_images() {
	IMAGE_TAG="$(source_tag)"
	local image file
	for image in gantry gantry-node-config; do
		if image_exists "$image" "$IMAGE_TAG"; then
			log "Reusing $REGISTRY/$image:$IMAGE_TAG"
			continue
		fi
		file="images/$image/Containerfile"
		log "Building $image with ACR Tasks"
		az_cmd acr build --registry "$ACR" --file "$file" \
			--image "$image:$IMAGE_TAG" --platform linux/amd64 \
			--build-arg BUILDPLATFORM=linux/amd64 --build-arg TARGETOS=linux \
			--build-arg TARGETARCH=amd64 --build-arg "VERSION=$IMAGE_TAG" \
			--build-arg "GIT_COMMIT=$(git -C "$REPO_ROOT" rev-parse HEAD)" "$REPO_ROOT"
	done
}

ensure_cluster() {
	local existing pools count enabled
	existing="$(az_cmd aks list --resource-group "$RESOURCE_GROUP" --query "[?name=='$CLUSTER'].id | [0]" --output tsv)"
	if [[ -z "$existing" ]]; then
		[[ -f "$STATE_DIR/ssh.pub" ]] || ssh-keygen -q -t rsa -b 3072 -N '' -f "$STATE_DIR/ssh"
		az_cmd aks create --name "$CLUSTER" --resource-group "$RESOURCE_GROUP" \
			--location "$LOCATION" --node-count 1 --nodepool-name system \
			--node-vm-size "$NODE_VM_SIZE" --os-sku Ubuntu \
			--network-plugin azure --network-plugin-mode overlay \
			--attach-acr "$ACR" --ssh-key-value "$STATE_DIR/ssh.pub" --output none
	fi
	pools="$(az_cmd aks nodepool list --cluster-name "$CLUSTER" --resource-group "$RESOURCE_GROUP" --output json)"
	count="$(printf '%s' "$pools" | python3 -c 'import json,sys; print(next((p["count"] for p in json.load(sys.stdin) if p["name"] == "system"), 0))')"
	[[ "$count" == 1 ]] || die "existing system pool must have exactly one node; refusing to scale it down"
	count="$(printf '%s' "$pools" | python3 -c 'import json,sys; print(next((p["count"] for p in json.load(sys.stdin) if p["name"] == "stream"), 0))')"
	if [[ "$count" == 0 ]]; then
		az_cmd aks nodepool add --cluster-name "$CLUSTER" --resource-group "$RESOURCE_GROUP" \
			--name stream --node-count "$WORKERS" --node-vm-size "$NODE_VM_SIZE" \
			--os-sku Ubuntu --enable-artifact-streaming \
			--labels gantry-streaming=true --output none
	else
		enabled="$(printf '%s' "$pools" | python3 -c 'import json,sys; print(str(next(p for p in json.load(sys.stdin) if p["name"] == "stream").get("artifactStreamingProfile", {}).get("enabled", False)).lower())')"
		[[ "$enabled" == true ]] || die "existing stream pool does not have artifact streaming enabled"
		if [[ "$count" -lt "$WORKERS" ]]; then
			az_cmd aks nodepool scale --cluster-name "$CLUSTER" --resource-group "$RESOURCE_GROUP" \
				--name stream --node-count "$WORKERS" --output none
		elif [[ "$count" -gt "$WORKERS" ]]; then
			die "existing worker count exceeds --nodes; refusing to drain nodes automatically"
		fi
	fi
	az_cmd aks get-credentials --name "$CLUSTER" --resource-group "$RESOURCE_GROUP" \
		--file "$STATE_DIR/kubeconfig" --overwrite-existing --output none
	kube wait nodes --selector gantry-streaming=true --for=condition=Ready --timeout=20m
}

ensure_results_storage() {
	ensure_group "$RESULTS_GROUP"
	local existing account_id principal assignments
	existing="$(az_cmd storage account list --resource-group "$RESULTS_GROUP" --query "[?name=='$STORAGE_ACCOUNT'].id | [0]" --output tsv)"
	if [[ -z "$existing" ]]; then
		az_cmd storage account create --name "$STORAGE_ACCOUNT" --resource-group "$RESULTS_GROUP" \
			--location "$LOCATION" --sku Standard_LRS --kind StorageV2 \
			--https-only true --min-tls-version TLS1_2 --output none
	fi
	account_id="$(az_cmd storage account show --name "$STORAGE_ACCOUNT" --resource-group "$RESULTS_GROUP" --query id --output tsv)"
	principal="$(az_cmd account get-access-token --query accessToken --output tsv | python3 -c 'import base64,json,sys; token=sys.stdin.read().strip().split(".")[1]; print(json.loads(base64.urlsafe_b64decode(token + "=" * (-len(token) % 4)))["oid"])')"
	assignments="$(az_cmd role assignment list --assignee "$principal" --scope "$account_id" --include-inherited \
		--query "[?roleDefinitionName=='Storage Blob Data Contributor' || roleDefinitionName=='Storage Blob Data Owner'] | length(@)" --output tsv)"
	if [[ "$assignments" == 0 ]]; then
		az_cmd role assignment create --assignee-object-id "$principal" --assignee-principal-type User \
			--role 'Storage Blob Data Contributor' --scope "$account_id" --output none
	fi
	PUBLISH_READY=true
}

install_gantry() {
	STAGE=installing-gantry
	python3 - "$RUN_DIR/values.json" "$REGISTRY" "$IMAGE_TAG" <<'PY'
import json, sys
path, registry, tag = sys.argv[1:]
selector = {"gantry-streaming": "true"}
values = {
    "image": {"reference": f"{registry}/gantry:{tag}"},
    "nodeSelector": selector,
    "overlaybdConfig": {
        "enabled": True, "nodeSelector": selector,
        "image": {"reference": f"{registry}/gantry-node-config:{tag}"}
    },
    "gantry": {
        "artifactStreaming": {"enabled": True},
        "upstreamRegistries": [
            {"name": registry, "endpoint": f"http://127.0.0.1:8578?ns={registry}"}
        ]
    }
}
with open(path, "w") as output:
    json.dump(values, output, indent=2)
PY
	local deployed
	deployed="$("$HELM" list --kubeconfig "$STATE_DIR/kubeconfig" --namespace "$GANTRY_NAMESPACE" --filter '^gantry$' --output json)"
	if [[ "$deployed" != '[]' ]]; then
		"$HELM" get values gantry --kubeconfig "$STATE_DIR/kubeconfig" --namespace "$GANTRY_NAMESPACE" --output json > "$RUN_DIR/previous-values.json"
		if ! python3 - "$RUN_DIR/values.json" "$RUN_DIR/previous-values.json" <<'PY'
import json, sys
with open(sys.argv[1]) as desired, open(sys.argv[2]) as previous:
    raise SystemExit(0 if json.load(desired) == json.load(previous) else 1)
PY
		then
			die "Gantry values changed; drain streaming workloads and uninstall Gantry before replacing its snapshotter image"
		fi
	fi
	"$HELM" upgrade --install gantry "$REPO_ROOT/deploy/gantry/chart" \
		--kubeconfig "$STATE_DIR/kubeconfig" --namespace "$GANTRY_NAMESPACE" --create-namespace \
		--values "$RUN_DIR/values.json" --wait --timeout 15m
	verify_gantry
}

verify_gantry() {
	local name pod selected
	for name in gantry gantry-containerd-config gantry-overlaybd-config; do
		kube --namespace "$GANTRY_NAMESPACE" rollout status "daemonset/$name" --timeout=15m
	done
	kube get nodes --selector gantry-streaming=true --output json > "$RUN_DIR/worker-nodes.json"
	[[ "$(json_field items < "$RUN_DIR/worker-nodes.json" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')" == "$WORKERS" ]] || die "wrong number of Gantry workers"
	kube --namespace "$GANTRY_NAMESPACE" get pods \
		--selector app.kubernetes.io/component=overlaybd-config --output jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' > "$RUN_DIR/config-pods.txt"
	while IFS= read -r pod; do
		[[ -n "$pod" ]] || continue
		# Expansion belongs to the remote shell and awk, not this workstation.
		# shellcheck disable=SC2016
		selected="$(kube --namespace "$GANTRY_NAMESPACE" exec "$pod" --container configure -- \
			nsenter -t 1 -m -u -i -n -p --root=/proc/1/root --wd=/ -- sh -c \
			'containerd config dump | awk '"'"'
			/^\[/ { header=$0; gsub(/[\047" \t]/, "", header); selected=(header=="[plugins.io.containerd.cri.v1.images]" || header=="[plugins.io.containerd.grpc.v1.cri.containerd]") }
			selected && $1=="snapshotter" { value=$3; gsub(/[\047"]/, "", value); print value }
			'"'"'')"
		[[ "$selected" == gantry-overlaybd ]] || die "$pod: effective CRI snapshotter is $selected, not gantry-overlaybd"
	done < "$RUN_DIR/config-pods.txt"
}

generate_fixtures() {
	STAGE=generating-fixtures
	(
		cd "$RUN_DIR"
		bash "$ACR_MIRROR_DIR/tools/aks/create-yaml-pod-files-amd64.sh" "$MONIKER" Ubuntu
	) > "$RUN_DIR/generator.log"
	local batch
	for batch in deploy-test-pods deploy-test-pods-batch2; do
		kube create --dry-run=client --validate=false --filename "$RUN_DIR/$batch.yaml" --output json > "$RUN_DIR/$batch-original.json"
	done
	python3 - "$RUN_DIR" "$REGISTRY" "$IMAGE_MAP" "$MONIKER" "$TEST_NAMESPACE" "$OWNER" <<'PY'
import json, pathlib, re, sys
directory, registry, map_path, moniker, namespace, owner = sys.argv[1:]
root = pathlib.Path(directory)
overrides = {}
if map_path:
    with open(map_path) as source:
        overrides = json.load(source)
    if not isinstance(overrides, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in overrides.items()):
        raise SystemExit("image map must be a JSON object of original image refs to source image refs")
fixtures = {}
for batch in ("deploy-test-pods", "deploy-test-pods-batch2"):
    with open(root / f"{batch}-original.json") as source:
        document = json.load(source)
    pods = document.get("items", [document])
    for pod in pods:
        labels = pod["metadata"].setdefault("labels", {})
        labels.update({"app.kubernetes.io/managed-by": owner, "gantry-e2e-run": moniker})
        pod["metadata"]["namespace"] = namespace
        pod["spec"]["topologySpreadConstraints"] = [{
            "maxSkew": 1, "topologyKey": "kubernetes.io/hostname",
            "whenUnsatisfiable": "ScheduleAnyway",
            "labelSelector": {"matchLabels": {"gantry-e2e-run": moniker}}
        }]
        for container in pod["spec"]["containers"]:
            original = container["image"]
            host, image = original.split("/", 1)
            source = overrides.get(original, original)
            if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._/:@-]+", source):
                raise SystemExit("source image references must not contain credentials or shell syntax")
            repository = "fixtures/" + host.split(".")[0] + "/" + image.rsplit(":", 1)[0]
            tag = image.rsplit(":", 1)[1] if ":" in image else "latest"
            destination = repository + ":" + tag
            streaming = host in ("teleportmetricstest.azurecr.io", "dpteleportmetricstest.azurecr.io") and not image.startswith("non/")
            fixtures[original] = {"original": original, "source": source, "destination": destination, "repository": repository, "tag": tag, "streaming": streaming}
            container["image"] = registry + "/" + destination
    with open(root / f"{batch}.json", "w") as output:
        json.dump({"apiVersion": "v1", "kind": "List", "items": pods}, output, indent=2)
unknown = set(overrides) - set(fixtures)
if unknown:
    raise SystemExit("image map contains unknown fixture refs: " + ", ".join(sorted(unknown)))
with open(root / "fixtures.json", "w") as output:
    json.dump(list(fixtures.values()), output, indent=2)
with open(root / "fixtures.tsv", "w") as output:
    for fixture in fixtures.values():
        output.write("\t".join(str(fixture[k]).lower() if isinstance(fixture[k], bool) else fixture[k]
                               for k in ("source", "destination", "repository", "tag", "streaming")) + "\n")
PY
}

prepare_fixtures() {
	STAGE=importing-fixtures
	local source destination repository tag streaming status deadline
	while IFS=$'\t' read -r source destination repository tag streaming; do
		if ! image_exists "$repository" "$tag"; then
			log "Importing $source into $REGISTRY/$destination"
			az_cmd acr import --name "$ACR" --source "$source" --image "$destination" --output none
		fi
		if [[ "$streaming" == true ]]; then
			az_cmd acr artifact-streaming update --name "$ACR" --repository "$repository" \
				--enable-streaming true --output json > "$RUN_DIR/conversion-update.json"
			az_cmd acr artifact-streaming create --name "$ACR" --image "$destination" \
				--output json > "$RUN_DIR/conversion-create.json"
			deadline=$((SECONDS + 3600))
			while :; do
				az_cmd acr artifact-streaming operation show --name "$ACR" --image "$destination" \
					--output json > "$RUN_DIR/conversion-status.json"
				status="$(json_field status < "$RUN_DIR/conversion-status.json")"
				case "$status" in
				Succeeded) break ;;
				Failed|Canceled|Cancelled) die "artifact conversion failed for $destination: $status" ;;
				esac
				[[ "$SECONDS" -lt "$deadline" ]] || die "artifact conversion timed out for $destination"
				sleep 15
			done
		fi
	done < "$RUN_DIR/fixtures.tsv"
}

collect_metrics() {
	local pod
	kube --namespace "$GANTRY_NAMESPACE" get pods --selector app.kubernetes.io/component=agent \
		--output jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' > "$RUN_DIR/agent-pods.txt"
	: > "$RUN_DIR/$1.prom"
	while IFS= read -r pod; do
		[[ -n "$pod" ]] || continue
		kube get --raw "/api/v1/namespaces/$GANTRY_NAMESPACE/pods/$pod:9095/proxy/metrics" >> "$RUN_DIR/$1.prom"
	done < "$RUN_DIR/agent-pods.txt"
}

execute_fixtures() {
	STAGE=testing
	kube create namespace "$TEST_NAMESPACE" --dry-run=client --output yaml | kube apply --filename -
	kube label nodes --selector gantry-streaming=true "version=$MONIKER" --overwrite
	kube --namespace "$TEST_NAMESPACE" delete pods \
		--selector "app.kubernetes.io/managed-by=$OWNER" --wait=true --timeout=5m
	collect_metrics before
	kube apply --filename "$RUN_DIR/deploy-test-pods.json"
	local name failed=0
	for name in redis quartus sagemathinc testmulti dp-jupyter napari; do
		if ! kube --namespace "$TEST_NAMESPACE" wait "pod/$name-obd-$MONIKER" \
			--for=condition=Ready --timeout="$POD_TIMEOUT"; then
			printf 'error: required workload failed: %s\n' "$name" >&2
			failed=1
		fi
	done
	sleep "$BATCH_DELAY"
	kube apply --filename "$RUN_DIR/deploy-test-pods-batch2.json"
	if ! kube --namespace "$TEST_NAMESPACE" wait "pod/jupyter-obd-batch2-$MONIKER" \
		--for=condition=Ready --timeout="$POD_TIMEOUT"; then
		printf 'error: required batch-2 workload failed\n' >&2
		failed=1
	fi
	collect_metrics after
	collect_pods
	[[ "$failed" == 0 ]] || die "one or more existing workload assertions failed"
	python3 - "$RUN_DIR/before.prom" "$RUN_DIR/after.prom" <<'PY'
import re, sys
def successful_ranges(path):
    total = 0.0
    with open(path) as source:
        for line in source:
            if line.startswith("gantry_streaming_requests_total{") and 'outcome="success"' in line:
                total += float(line.rsplit(" ", 1)[1])
    return total
if successful_ranges(sys.argv[2]) <= successful_ranges(sys.argv[1]):
    raise SystemExit("workloads did not produce successful Gantry streaming range requests")
PY
	RESULT=passed
}

collect_pods() {
	kube --namespace "$TEST_NAMESPACE" get pods --selector "gantry-e2e-run=$MONIKER" --output json > "$RUN_DIR/pods.json"
	python3 - "$RUN_DIR" <<'PY'
import datetime, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
with open(root / "pods.json") as source:
    pods = json.load(source)["items"]
def epoch(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
rows, outcomes = [], []
for pod in pods:
    status = pod.get("status", {})
    statuses = {c["name"]: c for c in status.get("containerStatuses", [])}
    ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", []))
    outcomes.append({"pod": pod["metadata"]["name"], "node": pod["spec"].get("nodeName"), "phase": status.get("phase"), "ready": ready})
    for container in pod["spec"]["containers"]:
        current = statuses.get(container["name"], {})
        started = current.get("state", {}).get("running", {}).get("startedAt")
        start = status.get("startTime")
        rows.append({
            "pod": pod["metadata"]["name"], "container": container["name"],
            "image": container["image"], "digest": current.get("imageID", "").split("@")[-1],
            "os": "Ubuntu", "podStartTime": start, "containerStartedAt": started,
            "startupTimeSeconds": int(epoch(started) - epoch(start)) if start and started else None
        })
with open(root / "publish" / "results.jsonl", "w") as output:
    for row in rows:
        output.write(json.dumps(row) + "\n")
with open(root / "pod-outcomes.json", "w") as output:
    json.dump(outcomes, output, indent=2)
PY
}

write_summary() {
	local rc="$1"
	python3 - "$RUN_DIR" "$rc" "$RESULT" "$STAGE" "$SUBSCRIPTION" "$RESOURCE_GROUP" "$RUN_ID" "$NODE_COUNT" "$REPO_ROOT" "$ACR_MIRROR_DIR" <<'PY'
import datetime, json, pathlib, subprocess, sys
directory, code, result, stage, subscription, group, run_id, count, repo, mirror = sys.argv[1:]
root = pathlib.Path(directory)
def revision(path):
    return subprocess.check_output(["git", "-C", path, "rev-parse", "HEAD"], text=True).strip()
summary = {
    "runId": run_id, "status": result if code == "0" else "failed",
    "exitCode": int(code), "stage": stage, "subscription": subscription,
    "resourceGroup": group, "nodeCount": int(count),
    "snapshotter": "gantry-overlaybd", "unboundedCommit": revision(repo),
    "acrMirrorCommit": revision(mirror),
    "recordedAt": datetime.datetime.now(datetime.timezone.utc).isoformat()
}
if (root / "pod-outcomes.json").exists():
    with open(root / "pod-outcomes.json") as source:
        summary["pods"] = json.load(source)
with open(root / "summary.json", "w") as output:
    json.dump(summary, output, indent=2)
PY
}

publish_results() {
	STAGE=publishing
	az_cmd storage blob service-properties update --account-name "$STORAGE_ACCOUNT" \
		--auth-mode login --static-website --index-document index.html --404-document index.html --output none
	local file exists etag
	for file in index.html baseline.json; do
		az_cmd storage blob upload --account-name "$STORAGE_ACCOUNT" --container-name "\$web" \
			--auth-mode login --name "results/$file" --file "$ACR_MIRROR_DIR/tools/dashboard/$file" \
			--overwrite true --content-type "$( [[ "$file" == *.html ]] && printf text/html || printf application/json )" --output none
	done
	az_cmd storage blob upload --account-name "$STORAGE_ACCOUNT" --container-name "\$web" \
		--auth-mode login --name "results/$RUN_ID/summary.json" --file "$RUN_DIR/summary.json" \
		--overwrite true --content-type application/json --output none
	if [[ -f "$RUN_DIR/publish/results.jsonl" ]]; then
		az_cmd storage blob upload --account-name "$STORAGE_ACCOUNT" --container-name "\$web" \
			--auth-mode login --name "results/$RUN_ID/results.jsonl" --file "$RUN_DIR/publish/results.jsonl" \
			--overwrite true --content-type application/json --output none
	fi
	exists="$(az_cmd storage blob exists --account-name "$STORAGE_ACCOUNT" --container-name "\$web" \
		--auth-mode login --name results/versions.json --query exists --output tsv)"
	local condition=()
	if [[ "$exists" == true ]]; then
		etag="$(az_cmd storage blob download --account-name "$STORAGE_ACCOUNT" --container-name "\$web" \
			--auth-mode login --name results/versions.json --file "$RUN_DIR/versions.json" --overwrite \
			--query properties.etag --output tsv)"
		condition=(--if-match "$etag")
	else
		printf '[]\n' > "$RUN_DIR/versions.json"
		condition=(--if-none-match '*')
	fi
	python3 - "$RUN_DIR/versions.json" "$RUN_DIR/summary.json" <<'PY'
import json, sys
with open(sys.argv[1]) as source:
    versions = json.load(source)
with open(sys.argv[2]) as source:
    summary = json.load(source)
entry = {"version": summary["runId"], "timestamp": summary["recordedAt"], "targets": ["Ubuntu"], "status": summary["status"]}
versions = [entry] + [v for v in versions if v["version"] != entry["version"]]
with open(sys.argv[1], "w") as output:
    json.dump(versions[:50], output, indent=2)
PY
	az_cmd storage blob upload --account-name "$STORAGE_ACCOUNT" --container-name "\$web" \
		--auth-mode login --name results/versions.json --file "$RUN_DIR/versions.json" \
		--overwrite true "${condition[@]}" --content-type application/json --output none
	local endpoint
	endpoint="$(az_cmd storage account show --name "$STORAGE_ACCOUNT" --resource-group "$RESULTS_GROUP" --query primaryEndpoints.web --output tsv)"
	printf '%s\n' "${endpoint}results/index.html" > "$STATE_DIR/dashboard-url"
	log "Dashboard: ${endpoint}results/index.html"
	log "Run summary: ${endpoint}results/$RUN_ID/summary.json"
}

finish() {
	local rc=$?
	trap - EXIT
	if [[ -n "${RUN_DIR:-}" ]]; then
		if ! write_summary "$rc"; then
			printf 'error: failed to write run summary\n' >&2
			rc=1
		fi
		if [[ "$rc" != 0 && -f "$STATE_DIR/kubeconfig" ]]; then
			if ! kube --namespace "$GANTRY_NAMESPACE" get pods --output wide > "$RUN_DIR/gantry-pods.txt" 2>&1; then
				printf 'error: could not collect Gantry Pod diagnostics\n' >&2
			fi
			if [[ -f "$RUN_DIR/fixtures.json" ]] && ! collect_pods; then
				printf 'error: could not collect workload outcomes\n' >&2
			fi
		fi
		if [[ "$rc" != 0 && "$PUBLISH_READY" == true ]]; then
			if ! publish_results; then
				printf 'error: failed to publish failure summary; local results: %s\n' "$RUN_DIR" >&2
			fi
		fi
	fi
	if [[ "$LOCK_HELD" == true ]]; then
		rm "$STATE_DIR/lock/pid"
		rmdir "$STATE_DIR/lock"
	fi
	exit "$rc"
}

main() {
	parse_args "$@"
	initialize
	if [[ "$COMMAND" == cleanup ]]; then
		[[ "$(az_cmd group exists --name "$RESOURCE_GROUP" --output tsv)" == true ]] || { RESULT=absent; return; }
		[[ "$(az_cmd group show --name "$RESOURCE_GROUP" --query tags.gantryIntegration --output tsv)" == "$OWNER" ]] || die "refusing to delete an unowned resource group"
		az_cmd group delete --name "$RESOURCE_GROUP" --yes
		RESULT=deleted
		return
	fi
	if [[ "$COMMAND" == publish ]]; then
		[[ -f "$RUN_DIR/summary.json" ]] || die "no retained summary for --run-id $RUN_ID"
		publish_results
		RESULT=published
		return
	fi
	STAGE=provisioning
	ensure_group "$RESOURCE_GROUP"
	ensure_registry
	if [[ "$COMMAND" != test ]]; then
		ensure_images
		ensure_cluster
		install_gantry
	else
		[[ -f "$STATE_DIR/kubeconfig" ]] || die "run deploy before test"
		verify_gantry
	fi
	ensure_results_storage
	if [[ "$COMMAND" == deploy ]]; then
		RESULT=deployed
	else
		generate_fixtures
		prepare_fixtures
		execute_fixtures
	fi
	write_summary 0
	publish_results
	STAGE=complete
	log "Cluster retained. Rerun this entrypoint to converge it; cleanup preserves $RESULTS_GROUP."
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
