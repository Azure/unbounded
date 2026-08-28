// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"strings"
)

const cacheEvictorDaemonSetName = "gantry-benchmark-cache-evictor"

func (b *benchmark) evictPreparedImages(ctx context.Context, state benchmarkState) error {
	baselineImage, gantryImage, err := state.preparedImages()
	if err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"delete", "daemonset", cacheEvictorDaemonSetName,
		"--ignore-not-found=true", "--wait=true",
	); err != nil {
		return err
	}

	daemonSet := cacheEvictorDaemonSet(b.config.Namespace, b.config.nodeSelector(), state.WorkloadRepository, []string{baselineImage, gantryImage})
	if err := b.applyObject(ctx, daemonSet); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"rollout", "status", "daemonset/"+cacheEvictorDaemonSetName,
		"--timeout", b.config.RolloutTimeout.String(),
	); err != nil {
		return fmt.Errorf("wait for benchmark cache eviction: %w", err)
	}

	if err := b.validateBenchmarkDaemonSet(ctx, cacheEvictorDaemonSetName); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"delete", "daemonset", cacheEvictorDaemonSetName,
		"--wait=true",
	); err != nil {
		return err
	}

	return nil
}

func cacheEvictorDaemonSet(namespace string, nodeSelector map[string]string, repository string, images []string) map[string]any {
	command := `set -eu
namespace=k8s.io
lease_filter='labels."gantry.io/managed"==true,labels."gantry.io/repository"=='"${REPOSITORY}"

old_ifs=${IFS}
IFS='|'
for image in ${IMAGE_REFS}; do
	chroot /host crictl rmi "${image}" >/dev/null 2>&1 || true
done

for lease in $(chroot /host ctr -n "${namespace}" leases ls -q "${lease_filter}"); do
	chroot /host ctr -n "${namespace}" leases rm "${lease}"
done

for image in ${IMAGE_REFS}; do
	digest=${image##*@}
	for image_ref in $(chroot /host ctr -n "${namespace}" images ls -q "target.digest==${digest}"); do
		chroot /host ctr -n "${namespace}" images rm --sync "${image_ref}"
	done
  if chroot /host ctr -n "${namespace}" images ls -q | grep -Fqx "${image}"; then
    echo "benchmark image reference remains after eviction: ${image}" >&2
    exit 1
  fi
done

chroot /host ctr -n "${namespace}" content prune references >/dev/null 2>&1 || true

for image in ${IMAGE_REFS}; do
	digest=${image##*@}
	manifest_present=true
	for _ in $(seq 1 60); do
		if ! chroot /host ctr -n "${namespace}" content get "${digest}" >/dev/null 2>&1; then
			manifest_present=false
			break
		fi
		sleep 2
	done
	if [ "${manifest_present}" = true ]; then
    echo "benchmark image manifest remains after synchronous eviction: ${digest}" >&2
    exit 1
  fi
done
IFS=${old_ifs}

touch /tmp/ready
echo "evicted benchmark image pair"
exec sleep 2147483647
`

	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      cacheEvictorDaemonSetName,
			"namespace": namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":    cacheEvictorDaemonSetName,
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": cacheEvictorDaemonSetName}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": cacheEvictorDaemonSetName}},
				"spec": map[string]any{
					"hostPID":      true,
					"nodeSelector": nodeSelector,
					"tolerations":  []any{map[string]any{"operator": "Exists"}},
					"containers": []any{
						map[string]any{
							"name":    "evict",
							"image":   "mcr.microsoft.com/cbl-mariner/busybox:2.0",
							"command": []string{"sh", "-c", command},
							"env": []any{
								map[string]string{"name": "REPOSITORY", "value": repository},
								map[string]string{"name": "IMAGE_REFS", "value": strings.Join(images, "|")},
							},
							"volumeMounts": []any{map[string]any{"name": "host", "mountPath": "/host"}},
							"readinessProbe": map[string]any{
								"exec":          map[string]any{"command": []string{"test", "-f", "/tmp/ready"}},
								"periodSeconds": 2,
							},
							"securityContext": map[string]any{
								"privileged": true,
								"runAsUser":  0,
							},
						},
					},
					"volumes": []any{
						map[string]any{"name": "host", "hostPath": map[string]any{"path": "/", "type": "Directory"}},
					},
				},
			},
		},
	}
}
