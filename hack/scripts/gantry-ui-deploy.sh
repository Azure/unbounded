#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

usage() {
    cat <<'EOF'
Build, push, and install the Gantry dashboard.
Usage: hack/scripts/gantry-ui-deploy.sh --kubeconfig PATH --context NAME --values FILE [options]
   or: hack/scripts/gantry-ui-deploy.sh --cluster-state DIR --acr-name NAME [options]

--cluster-state uses an existing aks-flex-cluster.sh deployment. It configures
Azure workload identities, ACR read/push access, worker pull authentication, and
a single-platform runtime base in ACR. It enables image generation, installs
the UI, and verifies a 1 MB generate/push/Flex-pull workflow before opening a
local tunnel. The cleanup button is enabled, but cleanup never runs automatically.
It never creates a cluster or deletes demos during deployment.

  --image REPO        Image repository (default ghcr.io/vpatelsj/gantry-ui)
  --tag TAG           Unique image tag (default dev-COMMIT-TIMESTAMP)
  --platform PLATFORM linux/amd64 or linux/arm64 (default linux/amd64)
  --namespace NAME    Dedicated demo namespace (default gantry-demo)
  --release NAME      Helm release (default gantry-ui)
  --engine NAME       docker or podman (default first available)
  --build-dns IP      Override DNS for Podman build containers only
  --yes              Confirm Azure setup without prompting
  --no-port-forward  Close the verification tunnel and exit after full setup
  --no-reclaimer     Hide privileged cluster image cleanup in full setup
  --help             Show this help

Sign in to GHCR using the selected engine first. UI build artifacts go to GHCR;
runtime bases and generated images go to your ACR. Full setup requires anonymous
pull access to the GHCR UI package; it stops with instructions if it is private.
Managed worker ACR authentication may restart Flex kubelets without draining.
The cleanup button requires an explicit DELETE ALL DEMOS confirmation before
deleting dashboard demo workloads and reclaiming unused node images, not ACR images.
Without --cluster-state, --values retains the existing externally configured
identity/registry workflow and does not change Azure resources or open a tunnel.
EOF
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
kubeconfig= context= values= engine=
cluster_state= acr_name= build_dns=
scratch= state_lock= port_forward_pid=
assume_yes=false
serve=true
reclaimer=true
reclaimer_option=false
image=ghcr.io/vpatelsj/gantry-ui
tag= platform=linux/amd64 namespace=gantry-demo release=gantry-ui

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
log() { printf '==> %s\n' "$*" >&2; }

cleanup() {
    local status=$?
    trap - EXIT
    if [[ -n "$port_forward_pid" ]] && jobs -pr | grep -Fxq "$port_forward_pid"; then
        kill "$port_forward_pid"
    fi
    if [[ -n "$scratch" ]]; then
        rm -f "$scratch/digest" "$scratch/base-index.json" "$scratch/setup.json" \
            "$scratch/serviceaccounts.yaml" "$scratch/serviceaccounts.json" \
            "$scratch/accounts.json" "$scratch/response.json" "$scratch/port-forward.log" "$scratch/values.json"
        rmdir "$scratch"
    fi
    if [[ -n "$state_lock" ]]; then rmdir "$state_lock"; fi
    exit "$status"
}

kube() { kubectl --kubeconfig "$kubeconfig" --context "$context" "$@"; }
azure() { az "$@" --subscription "$subscription" --only-show-errors; }

render_chart() {
    "$helm_bin" template "$release" "$chart" --namespace "$namespace" --values "$values" \
        --set-string "image.repository=$image" --set-string "image.digest=$1" \
        --set-string "architecture=$arch" \
        --set-string "nodeSelector.kubernetes\\.io/arch=$arch" "${@:2}"
}

prepare_cluster_setup() {
    local cluster_json registry_json server actual_hash desired worker_count system_count
    subscription=$(jq -er '.subscription | strings | select(length > 0)' "$cluster_state/intent.json")
    cluster_name=$(jq -er '.name | strings | select(test("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"))' "$cluster_state/intent.json")
    resource_group=$(jq -er '.resourceGroup | strings | select(length > 0)' "$cluster_state/intent.json")
    desired=$(jq -er '.pluginSHA256 | strings | select(test("^[a-f0-9]{64}$"))' "$cluster_state/intent.json") \
        || die "full setup requires the saved checkout installer from aks-flex-cluster.sh"
    plugin="$cluster_state/bin/custom-$desired/kubectl-unbounded"
    [[ -x "$plugin" ]] || die "saved installer is missing: $plugin"
    actual_hash=$(openssl dgst -sha256 "$plugin" | awk '{print $NF}')
    [[ "$actual_hash" == "$desired" ]] || die "saved installer checksum does not match the cluster intent"

    cluster_json=$(azure aks show --name "$cluster_name" --resource-group "$resource_group" -o json)
    jq -e '.provisioningState == "Succeeded"' <<<"$cluster_json" >/dev/null \
        || die "AKS provisioning must be Succeeded before UI setup"
    cluster_id=$(jq -er '.id' <<<"$cluster_json")
    location=$(jq -er '.location' <<<"$cluster_json")
    server=$(kube config view --minify -o 'jsonpath={.clusters[0].cluster.server}')
    python3 - "$server" "$(jq -r '.fqdn' <<<"$cluster_json")" "$(jq -r '.privateFqdn // empty' <<<"$cluster_json")" <<'PY' \
        || die "selected kubeconfig/context does not match the saved AKS cluster"
from urllib.parse import urlparse
import sys
url = urlparse(sys.argv[1])
if url.scheme != "https" or not url.hostname or url.hostname.lower() not in {v.lower() for v in sys.argv[2:] if v}:
    sys.exit(1)
PY
    registry_json=$(azure acr show --name "$acr_name" -o json)
    acr_id=$(jq -er '.id' <<<"$registry_json")
    registry=$(jq -er '.loginServer | select(test("^[a-z0-9]{5,50}\\.azurecr\\.io$"))' <<<"$registry_json")
    role_mode=$(jq -r '.roleAssignmentMode // "LegacyRegistryPermissions"' <<<"$registry_json")
    case "$role_mode" in
        LegacyRegistryPermissions|AbacRepositoryPermissions) ;;
        *) die "unsupported ACR role-assignment mode: $role_mode" ;;
    esac

    worker_count=$(kube get nodes -l "kubernetes.azure.com/agentpool=flexpool,kubernetes.azure.com/nodepool-type=FlexNodes,kubernetes.io/os=linux,kubernetes.io/arch=$arch" -o json |
        jq -er '.items | length | select(. > 0)') || die "no matching Linux Flex workers were found"
    system_count=$(kube get nodes -l "kubernetes.azure.com/mode=system,kubernetes.io/os=linux,kubernetes.io/arch=$arch" -o json |
        jq -er '.items | length | select(. > 0)') || die "the UI and builder require a matching Linux system node"
    log "Target: $cluster_name; ACR: $registry; $worker_count Flex workers; $system_count system nodes"

    state_dir="$root/tmp/gantry-ui/$cluster_name/$namespace/$release"
    mkdir -p "$state_dir"
    chmod 0700 "$state_dir"
    mkdir "$state_dir/.lock" 2>/dev/null || die "another UI setup holds $state_dir/.lock"
    state_lock="$state_dir/.lock"
    jq -n --arg cluster "$cluster_id" --arg acr "$acr_id" --arg ns "$namespace" --arg release "$release" \
        '{clusterID:$cluster,acrID:$acr,namespace:$ns,release:$release}' > "$scratch/setup.json"
    if [[ -f "$state_dir/setup.json" ]]; then
        cmp -s "$state_dir/setup.json" "$scratch/setup.json" \
            || die "saved UI setup targets a different cluster, registry, or release"
    fi

    values="$scratch/values.json"
    jq -n --arg registry "$registry" --arg arch "$arch" --arg base "$registry/gantry-demo/runtime@$placeholder_digest" \
        --argjson reclaimer "$reclaimer" \
        '{registry:$registry,architecture:$arch,nodePool:"flexpool",gantryNamespace:"unbounded-system",
          azure:{clientID:"pending"},builder:{enabled:true,azure:{clientID:"pending"},base:$base},
          reclaimer:{enabled:$reclaimer}}' > "$values"
    render_chart "$placeholder_digest" --show-only templates/serviceaccounts.yaml > "$scratch/serviceaccounts.yaml"
    kube create --dry-run=client --validate=false -f "$scratch/serviceaccounts.yaml" -o json > "$scratch/serviceaccounts.json"
    jq -s --argjson reclaimer "$reclaimer" \
        '[.[] | if .kind == "List" then .items[] else . end | select(.kind == "ServiceAccount")]
        | if length == (if $reclaimer then 3 else 2 end) then .
          else error("unexpected dashboard/helper service account count") end' \
        "$scratch/serviceaccounts.json" > "$scratch/accounts.json"
    dashboard_sa=$(jq -er '[.[] | select(.metadata.name |
        (endswith("-builder") or endswith("-reclaimer")) | not)]
        | if length == 1 then .[0].metadata.name else error("ambiguous dashboard account") end' "$scratch/accounts.json")
    builder_sa=$(jq -er '[.[] | select(.metadata.name | endswith("-builder"))]
        | if length == 1 then .[0].metadata.name else error("ambiguous builder account") end' "$scratch/accounts.json")

    if [[ "$assume_yes" != true ]]; then
        printf 'This will configure Azure identities/ACR roles, managed Flex pull authentication,\n'
        printf 'and a 1 MB generation/push/pull test. Flex kubelets may restart without draining.\n'
        if [[ "$reclaimer" == true ]]; then
            printf 'The UI will expose privileged image cleanup; deployment does not run cleanup.\n'
        fi
        read -r -p 'Continue? [y/N] ' confirmation
        [[ "$confirmation" == y || "$confirmation" == Y ]] || die "UI setup canceled"
    fi
}

ensure_identity() {
    local name="$1" identity subject="$2" candidates credentials existing
    candidates=$(azure identity list --resource-group "$resource_group" -o json)
    identity=$(jq -c --arg name "$name" '[.[] | select(.name == $name)]
        | if length == 0 then null elif length == 1 then .[0] else error("ambiguous identity") end' <<<"$candidates")
    if [[ "$identity" == null ]]; then
        identity=$(azure identity create --name "$name" --resource-group "$resource_group" --location "$location" \
            --tags "unbounded-ui-cluster=$cluster_id" "unbounded-ui-release=$namespace/$release" -o json)
    else
        jq -e --arg cluster "$cluster_id" --arg release "$namespace/$release" \
            '.tags["unbounded-ui-cluster"] == $cluster and .tags["unbounded-ui-release"] == $release' <<<"$identity" >/dev/null \
            || die "identity $name exists but is not owned by this UI setup"
    fi
    credentials=$(azure identity federated-credential list --identity-name "$name" --resource-group "$resource_group" -o json)
    existing=$(jq -c '[.[] | select(.name == "gantry-ui")] | if length == 0 then null
        elif length == 1 then .[0] else error("ambiguous federation") end' <<<"$credentials")
    if [[ "$existing" == null ]]; then
        azure identity federated-credential create --identity-name "$name" --resource-group "$resource_group" \
            --name gantry-ui --issuer "$issuer" --subject "$subject" --audiences api://AzureADTokenExchange -o none
    else
        jq -e --arg issuer "$issuer" --arg subject "$subject" \
            '.issuer == $issuer and .subject == $subject and .audiences == ["api://AzureADTokenExchange"]' <<<"$existing" >/dev/null \
            || die "identity $name has a conflicting federated credential"
    fi
    configured_identity="$identity"
}

ensure_role() {
    local principal="$1" role="$2" definition assignments existing
    definition=$(azure role definition list --name "$role" -o json |
        jq -er '[.[] | select(.roleType == "BuiltInRole")] |
            if length == 1 then .[0].id else error("built-in role missing or ambiguous") end')
    assignments=$(azure role assignment list --scope "$acr_id" -o json)
    existing=$(jq -r --arg principal "$principal" --arg role "$definition" \
        '[.[] | select(.principalId == $principal and (.roleDefinitionId | ascii_downcase) == ($role | ascii_downcase)
            and ((.condition // "") == ""))] | length' <<<"$assignments")
    if [[ "$existing" == 0 ]]; then
        azure role assignment create --assignee-object-id "$principal" --assignee-principal-type ServicePrincipal \
            --role "$definition" --scope "$acr_id" -o none
    fi
}

configure_cluster() {
    local cluster_json dashboard_identity builder_identity base_digest actual_digest
    log "Configuring separate dashboard reader and image-builder identities"
    cluster_json=$(azure aks show --name "$cluster_name" --resource-group "$resource_group" -o json)
    if ! jq -e '.oidcIssuerProfile.enabled == true and .securityProfile.workloadIdentity.enabled == true' <<<"$cluster_json" >/dev/null; then
        azure aks update --name "$cluster_name" --resource-group "$resource_group" \
            --enable-oidc-issuer --enable-workload-identity -o none
        cluster_json=$(azure aks show --name "$cluster_name" --resource-group "$resource_group" -o json)
    fi
    issuer=$(jq -er '.oidcIssuerProfile.issuerUrl | strings | select(startswith("https://"))' <<<"$cluster_json")
    ensure_identity "$dashboard_sa-read" "system:serviceaccount:$namespace:$dashboard_sa"
    dashboard_identity="$configured_identity"
    ensure_identity "$dashboard_sa-build" "system:serviceaccount:$namespace:$builder_sa"
    builder_identity="$configured_identity"
    dashboard_client=$(jq -er '.clientId | strings | select(length > 0)' <<<"$dashboard_identity")
    builder_client=$(jq -er '.clientId | strings | select(length > 0)' <<<"$builder_identity")
    if [[ "$role_mode" == LegacyRegistryPermissions ]]; then
        ensure_role "$(jq -er '.principalId' <<<"$dashboard_identity")" AcrPull
        ensure_role "$(jq -er '.principalId' <<<"$builder_identity")" AcrPush
    else
        ensure_role "$(jq -er '.principalId' <<<"$dashboard_identity")" "Container Registry Repository Reader"
        ensure_role "$(jq -er '.principalId' <<<"$dashboard_identity")" "Container Registry Repository Catalog Lister"
        ensure_role "$(jq -er '.principalId' <<<"$builder_identity")" "Container Registry Repository Writer"
        ensure_role "$(jq -er '.principalId' <<<"$builder_identity")" "Container Registry Repository Catalog Lister"
    fi

    log "Ensuring current and future Flex workers can pull from $registry"
    "$plugin" gantry attach-acr --kubeconfig "$kubeconfig" --acr-resource-id "$acr_id" \
        --auth-mode aks-identity-binding --cluster-resource-id "$cluster_id" --timeout 15m

    log "Importing a digest-pinned $arch runtime base into ACR"
    if [[ "$engine" == podman ]]; then
        podman manifest inspect docker.io/library/busybox:1.37.0 > "$scratch/base-index.json"
    else
        docker buildx imagetools inspect --raw docker.io/library/busybox:1.37.0 > "$scratch/base-index.json"
    fi
    base_digest=$(jq -er --arg arch "$arch" '[.manifests[] |
        select(.platform.os == "linux" and .platform.architecture == $arch)] |
        if length == 1 then .[0].digest | select(test("^sha256:[a-f0-9]{64}$"))
        else error("runtime base platform missing or ambiguous") end' "$scratch/base-index.json")
    azure acr import --name "$acr_name" --source "docker.io/library/busybox@$base_digest" \
        --image "gantry-demo/runtime:$arch-${base_digest#sha256:}" --force -o none
    actual_digest=$(azure acr repository show --name "$acr_name" \
        --image "gantry-demo/runtime@$base_digest" --query digest -o tsv)
    [[ "$actual_digest" == "$base_digest" ]] || die "imported runtime base does not match its source digest"
    runtime_base="$registry/gantry-demo/runtime@$base_digest"
    values="$state_dir/values.json"
    jq -n --arg registry "$registry" --arg arch "$arch" --arg reader "$dashboard_client" \
        --arg builder "$builder_client" --arg base "$runtime_base" --argjson reclaimer "$reclaimer" \
        '{registry:$registry,architecture:$arch,nodePool:"flexpool",gantryNamespace:"unbounded-system",
          azure:{clientID:$reader},builder:{enabled:true,azure:{clientID:$builder},base:$base,
          repository:"gantry-demo/generated",concurrency:1},reclaimer:{enabled:$reclaimer}}' > "$values"
    cp "$scratch/setup.json" "$state_dir/setup.json"
}

verify_public_image() {
    local token repository="${image#ghcr.io/}"
    log "Checking anonymous access to the GHCR UI artifact"
    token=$(curl -fsS --connect-timeout 10 --max-time 30 --get \
        --data-urlencode service=ghcr.io --data-urlencode "scope=repository:$repository:pull" \
        https://ghcr.io/token | jq -er '.token | strings | select(length > 0)') \
        || die "GHCR UI package is not publicly pullable. Set $image to Public in GitHub package settings, then rerun."
    printf 'header = "Authorization: Bearer %s"\n' "$token" |
        curl --config - -fsS --head --connect-timeout 10 --max-time 30 \
            -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
            "https://ghcr.io/v2/$repository/manifests/$digest" >/dev/null \
        || die "published UI digest is not anonymously accessible; check GHCR package visibility"
}

verify_ui() {
    local deadline=$((SECONDS + 900)) port= build_name stage workload expected actual reference
    log "Opening a temporary UI tunnel for end-to-end verification"
    kubectl --kubeconfig "$kubeconfig" --context "$context" -n "$namespace" \
        port-forward --address 127.0.0.1 "deployment/$dashboard_sa" :8088 \
        > "$scratch/port-forward.log" 2>&1 &
    port_forward_pid=$!
    while [[ -z "$port" ]]; do
        if ! kill -0 "$port_forward_pid" 2>/dev/null; then
            cat "$scratch/port-forward.log" >&2
            die "UI port-forward exited before becoming ready"
        fi
        port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) -> 8088$/\1/p' "$scratch/port-forward.log" | head -n 1)
        ((SECONDS < deadline)) || die "timed out opening the UI tunnel"
        [[ -n "$port" ]] || sleep 1
    done
    ui_url="http://127.0.0.1:$port"
    curl -fsS --max-time 30 "$ui_url/api/config" > "$scratch/response.json"
    jq -e --arg registry "$registry" --argjson reclaimer "$reclaimer" \
        '.registry == $registry and .generation.enabled == true and .reclaim.enabled == $reclaimer' \
        "$scratch/response.json" >/dev/null || die "UI generation or cleanup capability does not match the requested configuration"
    log "Waiting for dashboard ACR catalog access and Azure role propagation"
    while true; do
        if curl -fsS --max-time 30 "$ui_url/api/images" > "$scratch/response.json" &&
            jq -e --arg registry "$registry" '.registry == $registry and (.errors | type == "array")
                and (.errors | length == 0) and (.images | type == "array")' "$scratch/response.json" >/dev/null; then
            break
        fi
        ((SECONDS < deadline)) || { cat "$scratch/response.json" >&2; die "dashboard cannot read its ACR catalog"; }
        sleep 10
    done
    log "Verifying a 1 MB image generation, ACR push, and Flex-worker pull"
    curl -fsS --max-time 30 -H 'Content-Type: application/json' \
        --data '{"layers":1,"layerMB":1,"streaming":false}' "$ui_url/api/builds" > "$scratch/response.json"
    build_name=$(jq -er '.name | select(test("^gantry-build-[a-f0-9]{20}$"))' "$scratch/response.json")
    deadline=$((SECONDS + 900))
    while true; do
        curl -fsS --max-time 30 "$ui_url/api/builds/$build_name" > "$scratch/response.json"
        stage=$(jq -er '.progress.stage' "$scratch/response.json")
        if [[ "$stage" == failed ]]; then
            cat "$scratch/response.json" >&2
            die "UI image generation/push failed"
        fi
        workload=$(jq -r '.deployment.name // empty' "$scratch/response.json")
        [[ -z "$workload" ]] || break
        ((SECONDS < deadline)) || { cat "$scratch/response.json" >&2; die "timed out waiting for UI image generation"; }
        sleep 5
    done
    reference=$(jq -er '.deployment.image.reference' "$scratch/response.json")
    [[ "$reference" == "$registry/gantry-demo/generated@sha256:"* && "$reference" =~ @sha256:[a-f0-9]{64}$ ]] \
        || die "generated image was not published into the expected ACR repository"
    actual=$(azure acr repository show --name "$acr_name" \
        --image "${reference#"$registry/"}" --query digest -o tsv)
    [[ "$actual" == "${reference##*@}" ]] || die "ACR did not return the generated image digest"
    kube -n "$namespace" wait --for=condition=complete "job/$build_name" --timeout=900s
    kube -n "$namespace" rollout status "daemonset/$workload" --timeout=900s
    expected=$(kube get nodes -l "kubernetes.azure.com/agentpool=flexpool,kubernetes.azure.com/nodepool-type=FlexNodes,kubernetes.io/os=linux,kubernetes.io/arch=$arch" -o json | jq -er '.items | length | select(. > 0)')
    kube -n "$namespace" get "daemonset/$workload" -o json |
        jq -e --argjson count "$expected" --arg image "$reference" \
            '.status.desiredNumberScheduled == $count and .status.updatedNumberScheduled == $count
             and .status.numberAvailable == $count and .status.observedGeneration >= .metadata.generation
             and .spec.template.spec.containers[0].image == $image' >/dev/null \
        || die "generated image is not available on every matching Flex worker"
    log "Verified: UI generated and pushed $reference; all $expected Flex workers pulled it"
    printf '\nValues: %s\nVerification workload: %s/%s\n' "$values" "$namespace" "$workload"
    if [[ "$serve" == true ]]; then
        printf 'Dashboard: %s\n' "$ui_url"
        printf 'Keep this terminal open. Press Ctrl-C to close only the local tunnel.\n'
        if wait "$port_forward_pid"; then
            die "UI tunnel closed unexpectedly"
        else
            cat "$scratch/port-forward.log" >&2
            die "UI tunnel failed"
        fi
    else
        printf 'Verification complete. Open a new local tunnel with:\n'
        printf 'kubectl --kubeconfig %q --context %q -n %q port-forward --address 127.0.0.1 deployment/%q 8088:8088\n' \
            "$kubeconfig" "$context" "$namespace" "$dashboard_sa"
    fi
}

while (($#)); do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --yes) assume_yes=true; shift ;;
        --no-port-forward) serve=false; shift ;;
        --no-reclaimer) reclaimer=false; reclaimer_option=true; shift ;;
        --kubeconfig|--context|--values|--image|--tag|--platform|--namespace|--release|--engine|--cluster-state|--acr-name|--build-dns)
            if (($# < 2)) || [[ "$2" == --* || -z "$2" ]]; then echo "Missing value for $1" >&2; exit 2; fi
            case "$1" in
                --kubeconfig) kubeconfig=$2 ;; --context) context=$2 ;;
                --values) values=$2 ;; --image) image=$2 ;; --tag) tag=$2 ;;
                --platform) platform=$2 ;; --namespace) namespace=$2 ;;
                --release) release=$2 ;; --engine) engine=$2 ;;
                --cluster-state) cluster_state=$2 ;; --acr-name) acr_name=$2 ;;
                --build-dns) build_dns=$2 ;;
            esac
            shift 2 ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done
if [[ -n "$cluster_state" ]]; then
    [[ -f "$cluster_state/intent.json" && -z "$values" && "$acr_name" =~ ^[a-zA-Z0-9]{5,50}$ ]] \
        || die "--cluster-state requires a saved intent, --acr-name, and no --values"
    cluster_state=$(cd "$cluster_state" && pwd)
    kubeconfig="${kubeconfig:-$cluster_state/kubeconfig}"
    [[ -f "$kubeconfig" ]] || die "cluster kubeconfig is missing"
elif [[ -n "$acr_name" || "$assume_yes" == true || "$serve" == false || "$reclaimer_option" == true ]]; then
    die "--acr-name, --yes, --no-port-forward, and --no-reclaimer require --cluster-state"
elif [[ ! -f "$kubeconfig" || ! -f "$values" || -z "$context" ]]; then
    echo "--kubeconfig, --context, and an existing --values file are required" >&2; exit 2
fi
case "$platform" in linux/amd64|linux/arm64) ;; *) echo "Unsupported platform: $platform" >&2; exit 2 ;; esac
if [[ ! "$image" =~ ^ghcr\.io/[a-z0-9][a-z0-9._/-]*$ ]]; then
    echo "--image must be an untagged lowercase ghcr.io repository" >&2; exit 2
fi
for name in "$namespace" "$release"; do
    if [[ ! "$name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ || ${#name} -gt 53 ]]; then
        echo "Invalid namespace or release name: $name" >&2; exit 2
    fi
done
if [[ -z "$engine" ]]; then
    if command -v docker >/dev/null 2>&1; then engine=docker
    elif command -v podman >/dev/null 2>&1; then engine=podman
    else echo "Docker or Podman is required; install/start one and retry" >&2; exit 1
    fi
fi
case "$engine" in docker|podman) ;; *) echo "--engine must be docker or podman" >&2; exit 2 ;; esac
if [[ -n "$build_dns" ]]; then
    [[ "$engine" == podman ]] || die "--build-dns is supported only with --engine podman"
    command -v python3 >/dev/null 2>&1 || die "python3 is required to validate --build-dns"
    python3 - "$build_dns" <<'PY' || die "--build-dns must be an IPv4 or IPv6 address"
import ipaddress
import sys
try:
    ipaddress.ip_address(sys.argv[1])
except ValueError:
    sys.exit(1)
PY
fi
for tool in "$engine" kubectl git; do
    if ! command -v "$tool" >/dev/null 2>&1; then echo "Required tool missing: $tool" >&2; exit 1; fi
done
if command -v helm >/dev/null 2>&1; then helm_bin=$(command -v helm)
elif [[ -x "$root/bin/helm" ]]; then helm_bin="$root/bin/helm"
else die "Helm is required; run make bin/helm or install Helm"
fi
if [[ -n "$cluster_state" ]]; then
    for tool in az jq python3 curl openssl; do
        command -v "$tool" >/dev/null 2>&1 || die "full UI setup requires $tool"
    done
fi
"$engine" info >/dev/null
if [[ -z "$tag" ]]; then tag="dev-$(git -C "$root" rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)"; fi
if [[ ! "$tag" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]]; then echo "Invalid image tag" >&2; exit 2; fi

chart="$root/deploy/gantry-ui/chart"
arch=${platform#linux/}
placeholder_digest="sha256:$(printf '%064d' 0)"
mkdir -p "$root/tmp"
umask 077
scratch=$(mktemp -d "$root/tmp/gantry-ui-push.XXXXXX")
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ -n "$cluster_state" && -z "$context" ]]; then
    context=$(kubectl --kubeconfig "$kubeconfig" config current-context)
    [[ -n "$context" ]] || die "cluster kubeconfig has no current context"
fi
kube cluster-info >/dev/null
if [[ -n "$cluster_state" ]]; then prepare_cluster_setup; fi
echo "Target context: $context; namespace: $namespace; image: $image:$tag"
# Validate chart configuration before building or publishing. The real digest
# is resolved from the pushed registry manifest, never the local image ID.
render_chart "$placeholder_digest" >/dev/null
if [[ "$engine" == docker ]]; then
    docker buildx build --platform "$platform" --file "$root/images/gantry-ui/Containerfile" \
        --build-arg TARGETOS=linux --build-arg "TARGETARCH=$arch" --tag "$image:$tag" --push "$root"
    manifest=$(docker buildx imagetools inspect "$image:$tag")
    digest=$(printf '%s\n' "$manifest" | awk '$1 == "Digest:" {print $2; exit}')
else
    build_args=()
    [[ -z "$build_dns" ]] || build_args+=(--dns "$build_dns")
    podman build --platform "$platform" --build-arg TARGETOS=linux --build-arg "TARGETARCH=$arch" \
        ${build_args[@]+"${build_args[@]}"} \
        --file "$root/images/gantry-ui/Containerfile" --tag "$image:$tag" "$root"
    podman push --digestfile "$scratch/digest" "$image:$tag"
    digest=$(<"$scratch/digest")
fi
if [[ ! "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]; then echo "Registry did not return a valid image digest" >&2; exit 1; fi
if [[ -n "$cluster_state" ]]; then
    verify_public_image
    configure_cluster
    render_chart "$digest" >/dev/null
fi
"$helm_bin" upgrade --install "$release" "$chart" --kubeconfig "$kubeconfig" --kube-context "$context" \
    --namespace "$namespace" --create-namespace --values "$values" \
    --set-string "image.repository=$image" --set-string "image.digest=$digest" \
    --set-string "architecture=$arch" \
    --set-string "nodeSelector.kubernetes\\.io/arch=$arch" --wait --timeout 15m
echo "Published $image@$digest"
if [[ -n "$cluster_state" ]]; then verify_ui
else echo "Use the port-forward command in Helm NOTES to open the dashboard."
fi
