package main

import (
	"context"
	"fmt"
)

func runClusterInfoPreflight(ctx context.Context, kubeconfigPath string, exec Executor) error {
	return exec.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath, "cluster-info")
}

func validateBMCSecretKeys(ctx context.Context, cfg preflightConfig, exec Executor, bmcSecretKeys []string) error {
	for _, bmcSecretKey := range bmcSecretKeys {
		bmcSecretValue, err := exec.Output(ctx, "kubectl", "--kubeconfig", cfg.KubeconfigPath, "-n", "unbounded-kube", "get", "secret", defaultBMCSecretName, "-o", "jsonpath={.data['"+bmcSecretKey+"']}")
		if err != nil {
			return fmt.Errorf("required BMC secret key %q missing from secret %q in namespace %q: %w", bmcSecretKey, defaultBMCSecretName, "unbounded-kube", err)
		}
		if bmcSecretValue == "" {
			return fmt.Errorf("required BMC secret key %q missing from secret %q in namespace %q", bmcSecretKey, defaultBMCSecretName, "unbounded-kube")
		}
	}

	return nil
}

func RunSetupPreflight(ctx context.Context, cfg SetupConfig, exec Executor) error {
	if err := runClusterInfoPreflight(ctx, cfg.KubeconfigPath, exec); err != nil {
		return err
	}

	if cfg.BMCSecretKey != "" {
		if err := validateBMCSecretKeys(ctx, preflightConfig{KubeconfigPath: cfg.KubeconfigPath}, exec, []string{cfg.BMCSecretKey}); err != nil {
			return err
		}
	}

	sshArgs := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "root@" + cfg.ProxmoxHost}
	pxeImage := cfg.PXEImage
	if pxeImage == "" {
		pxeImage = defaultPXEImage
	}

	checks := []struct {
		name string
		args []string
	}{
		{name: "ssh", args: append(append([]string{}, sshArgs...), "systemctl is-active proxmox-redfish")},
		{name: "ssh", args: append(append([]string{}, sshArgs...), "test -f /root/.kube/config")},
		{name: "ssh", args: append(append([]string{}, sshArgs...), "bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'")},
		{name: "ssh", args: append(append([]string{}, sshArgs...), "bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'")},
		{name: "ssh", args: append(append([]string{}, sshArgs...), "bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://"+pxeImage+" >/dev/null'")},
	}

	for _, check := range checks {
		if err := exec.Run(ctx, check.name, check.args...); err != nil {
			return err
		}
	}

	return nil
}

func RunPreflight(ctx context.Context, cfg preflightConfig, exec Executor) error {
	if cfg.SkipProxmox {
		return runClusterInfoPreflight(ctx, cfg.KubeconfigPath, exec)
	}
	return RunSetupPreflight(ctx, SetupConfig{
		CommonConfig: CommonConfig{KubeconfigPath: cfg.KubeconfigPath},
		ProxmoxHost:  cfg.ProxmoxHost,
		PXEImage:     cfg.PXEImage,
		BMCSecretKey: cfg.BMCSecretKey,
	}, exec)
}
