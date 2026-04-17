package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRunSetupPreflightRunsKubectlAndSSHChecks(t *testing.T) {
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                                                             nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish":                                            nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config":                                                     nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'":                                                                                              nil,
	}}

	err := RunSetupPreflight(context.Background(), SetupConfig{
		CommonConfig: CommonConfig{KubeconfigPath: "/root/.kube/config"},
		ProxmoxHost:  "10.10.100.2",
	}, exec)
	if err != nil {
		t.Fatalf("RunSetupPreflight() error = %v", err)
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestRunSetupPreflightUsesExplicitSharedBMCSecretKeyOverride(t *testing.T) {
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                                                             nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish":                                            nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config":                                                     nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'":                                                                                              nil,
	}, outputs: map[string]string{
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['shared-key']}": "c2VjcmV0",
	}}

	err := RunSetupPreflight(context.Background(), SetupConfig{
		CommonConfig: CommonConfig{KubeconfigPath: "/root/.kube/config"},
		ProxmoxHost:  "10.10.100.2",
		BMCSecretKey: "shared-key",
	}, exec)
	if err != nil {
		t.Fatalf("RunSetupPreflight() error = %v", err)
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['shared-key']}",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestPreflightFailsWhenRedfishCheckFails(t *testing.T) {
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                  nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish": errors.New("inactive"),
	}}

	err := RunPreflight(context.Background(), preflightConfig{ProxmoxHost: "10.10.100.2", KubeconfigPath: "/root/.kube/config"}, exec)
	if err == nil {
		t.Fatal("expected preflight error")
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestPreflightVerifyReadyOnlyChecksKubernetesConnectivity(t *testing.T) {
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info": nil,
	}}

	err := RunPreflight(context.Background(), preflightConfig{
		ProxmoxHost:    "10.10.100.2",
		KubeconfigPath: "/root/.kube/config",
		BMCSecretKey:   "shared-key",
		SkipProxmox:    true,
	}, exec)
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestPreflightSkipsPerNodeBMCSecretKeyValidationWithoutSharedOverride(t *testing.T) {
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                                                             nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish":                                            nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config":                                                     nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'":                                                                                              nil,
	}}

	err := RunPreflight(context.Background(), preflightConfig{ProxmoxHost: "10.10.100.2", KubeconfigPath: "/root/.kube/config"}, exec)
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestPreflightFailsWhenBMCSecretKeyIsMissing(t *testing.T) {
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info": nil,
	}, outputs: map[string]string{
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['stretch-pxe-0']}": "",
	}}

	err := RunPreflight(context.Background(), preflightConfig{ProxmoxHost: "10.10.100.2", KubeconfigPath: "/root/.kube/config", BMCSecretKey: "stretch-pxe-0"}, exec)
	if err == nil {
		t.Fatal("expected preflight error")
	}
	if err.Error() != "required BMC secret key \"stretch-pxe-0\" missing from secret \"bmc-passwords\" in namespace \"unbounded-kube\"" {
		t.Fatalf("error = %q", err.Error())
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['stretch-pxe-0']}",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestPreflightRunsKubectlAndSSHChecks(t *testing.T) {
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                                                             nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish":                                            nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config":                                                     nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'":                                                                                              nil,
	}}

	err := RunPreflight(context.Background(), preflightConfig{ProxmoxHost: "10.10.100.2", KubeconfigPath: "/root/.kube/config"}, exec)
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestPreflightUsesExplicitSharedBMCSecretKeyOverride(t *testing.T) {
	cfg := preflightConfig{
		ProxmoxHost:    "10.10.100.2",
		KubeconfigPath: "/root/.kube/config",
		BMCSecretKey:   "shared-key",
	}
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                                                             nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish":                                            nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config":                                                     nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'":                                                                                              nil,
	}, outputs: map[string]string{
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['shared-key']}": "c2VjcmV0",
	}}

	if err := RunPreflight(context.Background(), cfg, exec); err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	wantCalls := []string{
		"kubectl --kubeconfig /root/.kube/config cluster-info",
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['shared-key']}",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'",
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/azure/host-ubuntu2404:v0.0.13 >/dev/null'",
	}
	if !reflect.DeepEqual(exec.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", exec.calls, wantCalls)
	}
}

func TestPreflightUsesExplicitPXEImageOverride(t *testing.T) {
	overrideImage := "ghcr.io/example/custom-pxe:v9.9.9"
	cfg := preflightConfig{
		ProxmoxHost:    "10.10.100.2",
		KubeconfigPath: "/root/.kube/config",
		PXEImage:       overrideImage,
	}
	exec := &fakeExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                                                             nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish":                                            nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config":                                                     nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/example/custom-pxe:v9.9.9 >/dev/null'":                                                                                                  nil,
	}, outputs: map[string]string{
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['stretch-pxe-0']}": "c2VjcmV0",
		"kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['stretch-pxe-1']}": "c2VjcmV0",
	}}

	if err := RunPreflight(context.Background(), cfg, exec); err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	if exec.calls[len(exec.calls)-1] != "ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/example/custom-pxe:v9.9.9 >/dev/null'" {
		t.Fatalf("expected inspect to use override image, calls = %#v", exec.calls)
	}
	for _, call := range exec.calls {
		if call == "ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://"+defaultPXEImage+" >/dev/null'" {
			t.Fatalf("did not expect inspect to use default image %q, calls = %#v", defaultPXEImage, exec.calls)
		}
	}
}
