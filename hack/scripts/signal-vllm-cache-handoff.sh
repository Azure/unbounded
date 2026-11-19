#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 4 ]]; then
	echo "usage: $0 <namespace> <pvc> <seed-job> <timeout>" >&2
	exit 2
fi

namespace=$1
pvc=$2
seed_job=$3
timeout=$4

case "$namespace/$pvc/$seed_job" in
	*[!a-z0-9./-]*)
		echo "namespace, PVC, and seed Job names must be DNS-compatible" >&2
		exit 2
		;;
esac

if [[ ! "$timeout" =~ ^[1-9][0-9]*s$ ]]; then
	echo "timeout must be a positive whole number of seconds, for example 300s" >&2
	exit 2
fi

timeout_seconds=${timeout%s}
deadline=$((SECONDS + timeout_seconds))

remaining_timeout() {
	local remaining=$((deadline - SECONDS))
	if ((remaining <= 0)); then
		echo "timed out proving exclusive RWO model-cache ownership" >&2
		return 1
	fi

	printf '%ss' "$remaining"
}

job_uid=$(kubectl get -n "$namespace" "job/$seed_job" -o jsonpath='{.metadata.uid}')
if [[ -z "$job_uid" ]]; then
	echo "seed Job UID is empty" >&2
	exit 1
fi

pvc_uid=$(kubectl get -n "$namespace" "pvc/$pvc" -o jsonpath='{.metadata.uid}')
if [[ -z "$pvc_uid" ]]; then
	echo "model-cache PVC UID is empty" >&2
	exit 1
fi

printf 'seedJobUID=%s modelCachePVCUID=%s\n' "$job_uid" "$pvc_uid"

kubectl wait -n "$namespace" \
	--for=condition=complete \
	"job/$seed_job" \
	"--timeout=$(remaining_timeout)"
kubectl logs -n "$namespace" "job/$seed_job"

current_job_uid=$(kubectl get -n "$namespace" "job/$seed_job" -o jsonpath='{.metadata.uid}')
if [[ "$current_job_uid" != "$job_uid" ]]; then
	echo "seed Job UID changed before deletion: expected $job_uid, observed $current_job_uid" >&2
	exit 1
fi

printf '{"apiVersion":"v1","kind":"DeleteOptions","propagationPolicy":"Foreground","preconditions":{"uid":"%s"}}\n' \
	"$job_uid" |
	kubectl delete \
		--raw="/apis/batch/v1/namespaces/$namespace/jobs/$seed_job" \
		-f -

	current_pvc_uid=$(kubectl get -n "$namespace" "pvc/$pvc" -o jsonpath='{.metadata.uid}')
	if [[ "$current_pvc_uid" != "$pvc_uid" ]]; then
		echo "model-cache PVC UID changed before release proof: expected $pvc_uid, observed $current_pvc_uid" >&2
		exit 1
	fi

	pv=$(kubectl get -n "$namespace" "pvc/$pvc" -o jsonpath='{.spec.volumeName}')
	if [[ -z "$pv" ]]; then
	echo "model-cache PVC $namespace/$pvc is not bound to a PV" >&2
	exit 1
fi

absent_observed=false
while true; do
	current_job_uid=$(kubectl get -n "$namespace" "job/$seed_job" \
		--ignore-not-found \
		-o jsonpath='{.metadata.uid}')
	if [[ -n "$current_job_uid" && "$current_job_uid" != "$job_uid" ]]; then
		echo "seed Job was replaced during foreground deletion: expected $job_uid, observed $current_job_uid" >&2
		exit 1
	fi

	current_pvc_uid=$(kubectl get -n "$namespace" "pvc/$pvc" -o jsonpath='{.metadata.uid}')
	if [[ "$current_pvc_uid" != "$pvc_uid" ]]; then
		echo "model-cache PVC was replaced during release proof: expected $pvc_uid, observed $current_pvc_uid" >&2
		exit 1
	fi

	pod_refs=$(kubectl get pods -n "$namespace" -o json | python3 -c '
import json
import sys

pvc = sys.argv[1]
pods = json.load(sys.stdin).get("items", [])
references = []
for pod in pods:
    if any(
        volume.get("persistentVolumeClaim", {}).get("claimName") == pvc
        for volume in pod.get("spec", {}).get("volumes", [])
    ):
        references.append(pod.get("metadata", {}).get("name", "<unnamed>"))
print(",".join(sorted(references)))
' "$pvc")
	attachment_refs=$(kubectl get volumeattachments.storage.k8s.io -o json | python3 -c '
import json
import sys

pv = sys.argv[1]
attachments = json.load(sys.stdin).get("items", [])
references = [
    attachment.get("metadata", {}).get("name", "<unnamed>")
    for attachment in attachments
    if attachment.get("spec", {}).get("source", {}).get("persistentVolumeName") == pv
]
print(",".join(sorted(references)))
' "$pv")

	if [[ -z "$current_job_uid" && -z "$pod_refs" && -z "$attachment_refs" ]]; then
		if [[ "$absent_observed" == true ]]; then
			echo "exclusive RWO model-cache handoff proven for PVC $namespace/$pvc and PV $pv"
			exit 0
		fi

		absent_observed=true
	else
		absent_observed=false
	fi

	if ((SECONDS >= deadline)); then
		echo "timed out waiting for model-cache release: jobUID=[$current_job_uid] pods=[$pod_refs] volumeAttachments=[$attachment_refs]" >&2
		exit 1
	fi

	sleep 2
done
