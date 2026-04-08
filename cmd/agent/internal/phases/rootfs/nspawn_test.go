package rootfs

import (
	"bytes"
	"strings"
	"testing"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
)

func TestNSpawnTemplate_NoDevices(t *testing.T) {
	data := map[string]any{
		"KVMDevicePaths":       []string(nil),
		"NvidiaGPUDevicePaths": []string(nil),
		"NvidiaLibDirMounts":   []goalstates.NvidiaLibDirMount(nil),
	}

	var buf bytes.Buffer
	if err := nspawnTemplates.ExecuteTemplate(&buf, "nspawn.conf", data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	output := buf.String()

	// Should contain base config.
	if !strings.Contains(output, "Capability=all") {
		t.Error("expected Capability=all in output")
	}

	if !strings.Contains(output, "VirtualEthernet=no") {
		t.Error("expected VirtualEthernet=no in output")
	}

	// Should NOT contain [Files] section when no devices.
	if strings.Contains(output, "[Files]") {
		t.Error("unexpected [Files] section when no devices are present")
	}
}

func TestNSpawnTemplate_KVMOnly(t *testing.T) {
	data := map[string]any{
		"KVMDevicePaths":       []string{"/dev/kvm", "/dev/net/tun"},
		"NvidiaGPUDevicePaths": []string(nil),
		"NvidiaLibDirMounts":   []goalstates.NvidiaLibDirMount(nil),
	}

	var buf bytes.Buffer
	if err := nspawnTemplates.ExecuteTemplate(&buf, "nspawn.conf", data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "[Files]") {
		t.Error("expected [Files] section for KVM devices")
	}

	if !strings.Contains(output, "Bind=/dev/kvm") {
		t.Error("expected Bind=/dev/kvm")
	}

	if !strings.Contains(output, "Bind=/dev/net/tun") {
		t.Error("expected Bind=/dev/net/tun")
	}

	// Should NOT contain NVIDIA entries.
	if strings.Contains(output, "nvidia") {
		t.Error("unexpected nvidia reference when only KVM devices are present")
	}
}

func TestNSpawnTemplate_KVMAndNvidia(t *testing.T) {
	data := map[string]any{
		"KVMDevicePaths":       []string{"/dev/kvm"},
		"NvidiaGPUDevicePaths": []string{"/dev/nvidia0", "/dev/nvidiactl"},
		"NvidiaLibDirMounts": []goalstates.NvidiaLibDirMount{
			{Index: 0, HostDir: "/usr/lib/x86_64-linux-gnu", ContainerDir: "/run/host-nvidia/0"},
		},
	}

	var buf bytes.Buffer
	if err := nspawnTemplates.ExecuteTemplate(&buf, "nspawn.conf", data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	output := buf.String()

	// Only one [Files] section should be present.
	if count := strings.Count(output, "[Files]"); count != 1 {
		t.Errorf("expected exactly 1 [Files] section, got %d", count)
	}

	// KVM devices.
	if !strings.Contains(output, "Bind=/dev/kvm") {
		t.Error("expected Bind=/dev/kvm")
	}

	// NVIDIA devices.
	if !strings.Contains(output, "Bind=/dev/nvidia0") {
		t.Error("expected Bind=/dev/nvidia0")
	}

	if !strings.Contains(output, "Bind=/dev/nvidiactl") {
		t.Error("expected Bind=/dev/nvidiactl")
	}

	// NVIDIA library bind-mounts.
	if !strings.Contains(output, "BindReadOnly=/usr/lib/x86_64-linux-gnu:/run/host-nvidia/0") {
		t.Error("expected NVIDIA library bind-mount")
	}
}

func TestServiceOverrideTemplate_NoDevices(t *testing.T) {
	data := map[string]any{
		"KVMDevicePaths":       []string(nil),
		"NvidiaGPUDevicePaths": []string(nil),
	}

	var buf bytes.Buffer
	if err := nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "SYSTEMD_NSPAWN_UNIFIED_HIERARCHY=1") {
		t.Error("expected SYSTEMD_NSPAWN_UNIFIED_HIERARCHY=1")
	}

	if strings.Contains(output, "DeviceAllow") {
		t.Error("unexpected DeviceAllow when no devices are present")
	}
}

func TestServiceOverrideTemplate_KVMDevices(t *testing.T) {
	data := map[string]any{
		"KVMDevicePaths":       []string{"/dev/kvm", "/dev/net/tun"},
		"NvidiaGPUDevicePaths": []string(nil),
	}

	var buf bytes.Buffer
	if err := nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "DeviceAllow=/dev/kvm rwm") {
		t.Error("expected DeviceAllow=/dev/kvm rwm")
	}

	if !strings.Contains(output, "DeviceAllow=/dev/net/tun rwm") {
		t.Error("expected DeviceAllow=/dev/net/tun rwm")
	}
}

func TestServiceOverrideTemplate_KVMAndNvidia(t *testing.T) {
	data := map[string]any{
		"KVMDevicePaths":       []string{"/dev/kvm"},
		"NvidiaGPUDevicePaths": []string{"/dev/nvidia0"},
	}

	var buf bytes.Buffer
	if err := nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "DeviceAllow=/dev/kvm rwm") {
		t.Error("expected DeviceAllow=/dev/kvm rwm")
	}

	if !strings.Contains(output, "DeviceAllow=/dev/nvidia0 rwm") {
		t.Error("expected DeviceAllow=/dev/nvidia0 rwm")
	}
}
