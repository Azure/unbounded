// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type hostsMode string

const (
	hostsModeBaseline hostsMode = "baseline"
	hostsModeGantry   hostsMode = "gantry"
)

const (
	hostsConfigMapName = "gantry-benchmark-hosts"
	hostsDaemonSetName = "gantry-benchmark-hosts"
)

func renderHosts(state benchmarkState, mode hostsMode) (string, error) {
	marker := fmt.Sprintf("# Managed by Gantry benchmark %s.", state.RunID)

	switch mode {
	case hostsModeBaseline:
		return fmt.Sprintf(`%s
server = "http://%s:5002"

[host."http://%s:5002"]
  capabilities = ["pull", "resolve"]
`, marker, state.ProxyClusterIP, state.ProxyClusterIP), nil
	case hostsModeGantry:
		// STRICT mode: local Gantry is the ONLY upstream, with NO `server=`
		// fall-through. If Gantry returns 5xx (peer exhausted, starting up,
		// draining) containerd retries against Gantry rather than pulling the
		// blob straight from the proxy. This is what attributes the cold-phase
		// origin load to Gantry's pipeline cleanly: every byte the proxy sees
		// came from Gantry's own origin client (the designated puller), never
		// from a containerd-direct fall-through that would masquerade as
		// "origin load" and collapse the measured dedup whenever Gantry is
		// merely slow. Mirrors the upstream gantry benchmark methodology
		// (deploy/demo/hosts.toml.gantry-strict.template).
		//
		// With no `server=` line containerd derives ns=<registry-host> from the
		// certs.d directory name, which matches the upstream's `name` in
		// gantry-config directly (the ns_alias is only needed for the
		// server=<proxy> shape).
		return fmt.Sprintf(`%s
[host."http://127.0.0.1:5000"]
  capabilities = ["pull", "resolve"]
`, marker), nil
	default:
		return "", fmt.Errorf("unsupported hosts mode %q", mode)
	}
}

func (b *benchmark) installHosts(ctx context.Context, state benchmarkState, mode hostsMode) error {
	content, err := renderHosts(state, mode)
	if err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"delete", "daemonset", hostsDaemonSetName,
		"--ignore-not-found=true", "--wait=true",
	); err != nil {
		return err
	}

	configMap := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      hostsConfigMapName,
			"namespace": b.config.Namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":    hostsDaemonSetName,
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"data": map[string]string{"hosts.toml": content},
	}
	if err := b.applyObject(ctx, configMap); err != nil {
		return err
	}

	if err := b.applyObject(ctx, b.hostsInstallerDaemonSet(state)); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"rollout", "status", "daemonset/"+hostsDaemonSetName,
		"--timeout", b.config.RolloutTimeout.String(),
	); err != nil {
		return err
	}

	return b.validateBenchmarkDaemonSet(ctx, hostsDaemonSetName)
}

func (b *benchmark) hostsInstallerDaemonSet(state benchmarkState) map[string]any {
	command := `set -eu
target_dir="/host-certs/${REGISTRY_HOST}"
target="${target_dir}/hosts.toml"
active="${target_dir}/.gantry-benchmark-active"
backup="/host-state/${RUN_ID}"
marker="# Managed by Gantry benchmark ${RUN_ID}."
first_install=false

mkdir -p "${target_dir}" "${backup}"
if [ -e "${active}" ] && [ "$(cat "${active}")" != "${RUN_ID}" ]; then
  echo "another benchmark owns ${target_dir}" >&2
  exit 1
fi

if [ ! -e "${backup}/initialized" ]; then
  if [ -e "${active}" ]; then
    echo "benchmark ownership marker exists without backup state" >&2
    exit 1
  fi
  if [ -e "${target}" ]; then
		if grep -Fqx "${marker}" "${target}"; then
			echo "benchmark target exists without backup state" >&2
			exit 1
		fi
    cp "${target}" "${backup}/hosts.toml"
    printf 'present\n' > "${backup}/original-state"
	else
    printf 'absent\n' > "${backup}/original-state"
  fi
  printf '%s\n' "${RUN_ID}" > "${backup}/initialized"
  first_install=true
fi

if [ "${first_install}" = false ]; then
  if [ -e "${active}" ]; then
    if [ ! -e "${target}" ] || ! grep -Fqx "${marker}" "${target}"; then
      echo "benchmark-owned ${target} was changed or removed" >&2
      exit 1
    fi
	elif [ -e "${target}" ] && grep -Fqx "${marker}" "${target}"; then
		: # Recover an install interrupted after the atomic file move.
	else
    case "$(cat "${backup}/original-state")" in
      present)
        if [ ! -e "${target}" ] || ! cmp -s "${backup}/hosts.toml" "${target}"; then
          echo "cannot recover interrupted install because original ${target} changed" >&2
          exit 1
        fi
        ;;
      absent)
        if [ -e "${target}" ]; then
          echo "cannot recover interrupted install because ${target} appeared" >&2
          exit 1
        fi
        ;;
      *)
        echo "invalid original-state" >&2
        exit 1
        ;;
    esac
  fi
fi

temp="${target_dir}/.hosts.toml.gantry-benchmark.tmp"
cp /config/hosts.toml "${temp}"
chmod 0644 "${temp}"
mv "${temp}" "${target}"
printf '%s\n' "${RUN_ID}" > "${active}.tmp"
mv "${active}.tmp" "${active}"
touch /tmp/ready
echo "installed benchmark hosts.toml for ${REGISTRY_HOST}"
exec sleep 2147483647
`

	return nodeDaemonSet(
		b.config.Namespace,
		hostsDaemonSetName,
		b.config.nodeSelector(),
		command,
		map[string]string{
			"REGISTRY_HOST": state.ACRLoginServer,
			"RUN_ID":        state.RunID,
		},
		[]any{
			map[string]any{"name": "config", "mountPath": "/config", "readOnly": true},
		},
		[]any{
			map[string]any{"name": "config", "configMap": map[string]any{"name": hostsConfigMapName}},
		},
	)
}

func (b *benchmark) restoreHosts(ctx context.Context, state benchmarkState) error {
	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"delete", "daemonset", hostsDaemonSetName,
		"--ignore-not-found=true", "--wait=true",
	); err != nil {
		return err
	}

	name := "gantry-benchmark-hosts-restore"
	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"delete", "daemonset", name,
		"--ignore-not-found=true", "--wait=true",
	); err != nil {
		return err
	}

	command := `set -eu
target_dir="/host-certs/${REGISTRY_HOST}"
target="${target_dir}/hosts.toml"
active="${target_dir}/.gantry-benchmark-active"
backup="/host-state/${RUN_ID}"
marker="# Managed by Gantry benchmark ${RUN_ID}."

if [ ! -e "${backup}/initialized" ]; then
  if [ -e "${active}" ] || { [ -e "${target}" ] && grep -Fqx "${marker}" "${target}"; }; then
    echo "benchmark markers exist without backup state" >&2
    exit 1
  fi
  touch /tmp/ready
  echo "no benchmark host state found; nothing to restore"
  exec sleep 2147483647
fi

if [ -e "${active}" ] && [ "$(cat "${active}")" != "${RUN_ID}" ]; then
  echo "another benchmark owns ${target_dir}" >&2
  exit 1
fi

if [ -e "${active}" ]; then
  if [ -e "${target}" ] && grep -Fqx "${marker}" "${target}"; then
    : # Normal owned state.
  elif [ "$(cat "${backup}/original-state")" = present ] && [ -e "${target}" ] && cmp -s "${backup}/hosts.toml" "${target}"; then
    rm -f "${active}"
    rm -rf "${backup}"
    touch /tmp/ready
    echo "original hosts.toml was already restored"
    exec sleep 2147483647
  elif [ "$(cat "${backup}/original-state")" = absent ] && [ ! -e "${target}" ]; then
    rm -f "${active}"
    rmdir "${target_dir}" 2>/dev/null || true
    rm -rf "${backup}"
    touch /tmp/ready
    echo "absent hosts.toml was already restored"
    exec sleep 2147483647
  else
    echo "refusing to replace concurrently changed ${target}" >&2
    exit 1
  fi
elif [ -e "${target}" ] && grep -Fqx "${marker}" "${target}"; then
  : # Recover an install interrupted before the ownership marker was written.
else
  case "$(cat "${backup}/original-state")" in
    present)
      if [ -e "${target}" ] && cmp -s "${backup}/hosts.toml" "${target}"; then
        rm -rf "${backup}"
        touch /tmp/ready
        echo "original hosts.toml was already restored"
        exec sleep 2147483647
      fi
      ;;
    absent)
      if [ ! -e "${target}" ]; then
        rmdir "${target_dir}" 2>/dev/null || true
        rm -rf "${backup}"
        touch /tmp/ready
        echo "absent hosts.toml was already restored"
        exec sleep 2147483647
      fi
      ;;
  esac
  echo "backup exists without ownership marker and target is not restored" >&2
  exit 1
fi

orig_state="$(cat "${backup}/original-state")"
case "${orig_state}" in
  present)
    temp="${target_dir}/.hosts.toml.gantry-benchmark.restore"
    cp "${backup}/hosts.toml" "${temp}"
    chmod 0644 "${temp}"
    mv "${temp}" "${target}"
    ;;
  absent)
    rm -f "${target}"
    ;;
  *)
    echo "invalid original-state" >&2
    exit 1
    ;;
esac
rm -f "${active}"
if [ "${orig_state}" = absent ]; then
  rmdir "${target_dir}" 2>/dev/null || true
fi
rm -rf "${backup}"
touch /tmp/ready
echo "restored hosts.toml for ${REGISTRY_HOST}"
exec sleep 2147483647
`

	restorer := nodeDaemonSet(
		b.config.Namespace,
		name,
		b.config.nodeSelector(),
		command,
		map[string]string{
			"REGISTRY_HOST": state.ACRLoginServer,
			"RUN_ID":        state.RunID,
		},
		nil,
		nil,
	)
	if err := b.applyObject(ctx, restorer); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"rollout", "status", "daemonset/"+name,
		"--timeout", b.config.RolloutTimeout.String(),
	); err != nil {
		return err
	}

	if err := b.validateBenchmarkDaemonSetAtCurrentSize(ctx, name); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"delete", "daemonset", name, "--wait=true",
	); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"delete", "configmap", hostsConfigMapName,
		"--ignore-not-found=true",
	); err != nil {
		return err
	}

	return nil
}

func nodeDaemonSet(
	namespace,
	name string,
	nodeSelector map[string]string,
	command string,
	environment map[string]string,
	extraMounts,
	extraVolumes []any,
) map[string]any {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	env := make([]any, 0, len(keys))
	for _, key := range keys {
		env = append(env, map[string]string{"name": key, "value": environment[key]})
	}

	mounts := []any{
		map[string]any{"name": "host-certs", "mountPath": "/host-certs"},
		map[string]any{"name": "host-state", "mountPath": "/host-state"},
	}
	mounts = append(mounts, extraMounts...)
	volumes := []any{
		map[string]any{"name": "host-certs", "hostPath": map[string]any{"path": "/etc/containerd/certs.d", "type": "DirectoryOrCreate"}},
		map[string]any{"name": "host-state", "hostPath": map[string]any{"path": "/var/lib/gantry-benchmark", "type": "DirectoryOrCreate"}},
	}
	volumes = append(volumes, extraVolumes...)

	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":    name,
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": name}},
				"spec": map[string]any{
					"nodeSelector": nodeSelector,
					"tolerations":  []any{map[string]any{"operator": "Exists"}},
					"containers": []any{
						map[string]any{
							"name":         "configure",
							"image":        "mcr.microsoft.com/cbl-mariner/busybox:2.0",
							"command":      []string{"sh", "-c", command},
							"env":          env,
							"volumeMounts": mounts,
							"readinessProbe": map[string]any{
								"exec":          map[string]any{"command": []string{"test", "-f", "/tmp/ready"}},
								"periodSeconds": 2,
							},
							"securityContext": map[string]any{
								"runAsNonRoot":             false,
								"runAsUser":                0,
								"runAsGroup":               0,
								"allowPrivilegeEscalation": false,
								"capabilities": map[string]any{
									"drop": []string{"ALL"},
									"add":  []string{"DAC_OVERRIDE", "FOWNER"},
								},
								"seccompProfile": map[string]string{"type": "RuntimeDefault"},
							},
						},
					},
					"volumes": volumes,
				},
			},
		},
	}
}

func (b *benchmark) validateBenchmarkDaemonSet(ctx context.Context, name string) error {
	daemonSet, err := b.benchmarkDaemonSetStatus(ctx, name)
	if err != nil {
		return err
	}

	return validateBenchmarkDaemonSetStatus(daemonSet, name, b.config.NodeCount)
}

func (b *benchmark) validateBenchmarkDaemonSetAtCurrentSize(ctx context.Context, name string) error {
	daemonSet, err := b.benchmarkDaemonSetStatus(ctx, name)
	if err != nil {
		return err
	}

	if daemonSet.Status.DesiredNumberScheduled <= 0 {
		return fmt.Errorf("daemonset %s has no desired pods", name)
	}

	return validateBenchmarkDaemonSetStatus(daemonSet, name, daemonSet.Status.DesiredNumberScheduled)
}

func (b *benchmark) benchmarkDaemonSetStatus(ctx context.Context, name string) (daemonSetStatus, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"get", "daemonset", name, "-o", "json",
	)
	if err != nil {
		return daemonSetStatus{}, err
	}

	var daemonSet daemonSetStatus
	if err := json.Unmarshal(output, &daemonSet); err != nil {
		return daemonSetStatus{}, fmt.Errorf("decode DaemonSet %s: %w", name, err) //nolint:staticcheck // Kubernetes kind name starts with a capital.
	}

	return daemonSet, nil
}

func validateBenchmarkDaemonSetStatus(daemonSet daemonSetStatus, name string, expectedCount int) error {
	if daemonSet.Status.DesiredNumberScheduled != expectedCount || daemonSet.Status.NumberReady != expectedCount {
		return fmt.Errorf("daemonset %s is ready on %d/%d nodes", name, daemonSet.Status.NumberReady, expectedCount)
	}

	return nil
}
