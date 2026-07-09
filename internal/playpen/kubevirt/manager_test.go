// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kubevirt

import (
	"testing"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

func TestBootSpecPatchPXEPutsNetworkFirst(t *testing.T) {
	vm := &kubevirtv1.VirtualMachine{}
	applyBootSpec(vm, BootConfig{Target: BootTargetPxe, Enabled: BootContinuous}, "")
	devices := vm.Spec.Template.Spec.Domain.Devices

	if got := devices.Interfaces[0].BootOrder; got == nil || *got != 1 {
		t.Fatalf("network boot order = %v, want 1", got)
	}
	if got := devices.Disks[0].BootOrder; got == nil || *got != 2 {
		t.Fatalf("root boot order = %v, want 2", got)
	}
}

func TestBootSpecPatchHTTPUsesHelperDiskWhenConfigured(t *testing.T) {
	vm := &kubevirtv1.VirtualMachine{}
	applyBootSpec(vm, BootConfig{Target: BootTargetUefiHTTP, Enabled: BootOnce}, "example/httpboot:latest")
	disks := vm.Spec.Template.Spec.Domain.Devices.Disks

	if len(disks) != 3 {
		t.Fatalf("disk count = %d, want 3", len(disks))
	}
	if got := disks[2].BootOrder; got == nil || *got != 1 {
		t.Fatalf("http helper boot order = %v, want 1", got)
	}
}

func TestBuildVMUsesMultusBridgeAndUEFI(t *testing.T) {
	vm := buildVM(vmBuildInput{
		Name:                  "pp-a",
		Namespace:             "default",
		AllocationID:          "pp-a",
		Image:                 "image",
		NetworkAttachmentName: "default/playpen-net",
		MAC:                   "02:00:00:00:00:01",
	})

	efi := vm.Spec.Template.Spec.Domain.Firmware.Bootloader.EFI
	if efi == nil {
		t.Fatalf("efi firmware missing")
	}
	if efi.SecureBoot == nil || *efi.SecureBoot {
		t.Fatalf("expected secureBoot disabled for kind/emulation compatibility: %#v", efi.SecureBoot)
	}
	interfaces := vm.Spec.Template.Spec.Domain.Devices.Interfaces
	if len(interfaces) != 1 {
		t.Fatalf("interfaces missing: len=%d", len(interfaces))
	}
	iface := interfaces[0]
	if iface.Bridge == nil {
		t.Fatalf("bridge interface missing: %#v", iface)
	}
}

func TestSetBootConfigDisabledClearsHTTPBootURI(t *testing.T) {
	cfg := normalizeBootConfig(BootConfig{Target: BootTargetUefiHTTP, Enabled: BootDisabled, Mode: BootModeUEFI, HTTPBootURI: "http://example/boot.efi"})
	if cfg.Target != BootTargetHdd || cfg.Mode != "" || cfg.HTTPBootURI != "" {
		t.Fatalf("disabled config not normalized: %#v", cfg)
	}
}

func TestNormalizeBootConfigDefaultsHTTPMode(t *testing.T) {
	cfg := normalizeBootConfig(BootConfig{Target: BootTargetUefiHTTP, Enabled: BootOnce, HTTPBootURI: "http://example/boot.efi"})
	if cfg.Mode != BootModeUEFI || cfg.HTTPBootURI == "" {
		t.Fatalf("HTTP boot config not normalized: %#v", cfg)
	}
}
