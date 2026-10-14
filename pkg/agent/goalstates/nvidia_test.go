// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
)

const testNVIDIAQualificationDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func ptr[T any](value T) *T {
	return &value
}

func validTestNVIDIAQualification() NVIDIAIMEXQualification {
	return NVIDIAIMEXQualification{
		SchemaVersion:   1,
		Profile:         "hillsboro-gb200-gpu-operator-ready",
		ProfileVersion:  "v2026.10.14-1",
		ContractVersion: "hillsboro.nvidia-host-capabilities/v1",
		Architecture:    "arm64",
		Image: NVIDIAQualificationImage{
			ID:      ptr("hillsboro-azl3-gb200"),
			Version: ptr("2026.10.14"),
			Digest:  ptr("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		},
		Distro: NVIDIAQualificationDistro{
			Name:        "Azure Linux",
			Version:     "3.0",
			Kernel:      "6.18.48.1-1.azl3.aarch64",
			KernelDevel: "6.18.48.1-1.azl3.aarch64",
		},
		Driver: NVIDIAQualificationPackage{
			Branch:           "580",
			Version:          "580.159.04",
			UserspaceVersion: "580.159.04",
			Package:          "cuda-open-hwe-580.159.04-2_6.18.48.1.1.azl3.aarch64",
		},
		FabricManager: NVIDIAQualificationPackage{
			Version: "580.159.04",
			Package: "nvidia-fabric-manager-580.159.04-1.azl3.aarch64",
		},
		NSCQ: NVIDIAQualificationPackage{
			Version: "580.159.04",
			Package: "libnvidia-nscq-580.159.04-1.azl3.aarch64",
		},
		IMEX: NVIDIAQualificationIMEX{
			Package:       ptr("nvidia-imex-580.159.04-1.azl3.aarch64"),
			Version:       ptr("580.159.04"),
			ExpectedPaths: []string{"/usr/bin/nvidia-imex", "/usr/bin/nvidia-imex-ctl"},
			Service: NVIDIAQualificationService{
				Name:   "nvidia-imex.service",
				Masked: true,
			},
			LifecycleOwner: "gpu-operator-dra-driverManaged",
		},
		Toolkit: NVIDIAQualificationToolkit{
			Version:        ptr("1.19.1"),
			MinimumVersion: "1.18.0",
			Ready:          true,
		},
		ContainerRuntime: NVIDIAQualificationContainerRuntime{
			Name:               "containerd",
			Version:            ptr("2.0.6"),
			MinimumVersion:     "2.0.0",
			CDIEnabled:         true,
			CDISpecDirectories: []string{"/etc/cdi", "/var/run/cdi"},
		},
		Prerequisites: NVIDIAQualificationPrerequisites{
			RequiredFiles:         []string{"/usr/bin/nvidia-smi", "/usr/bin/nvidia-imex", "/usr/bin/nvidia-imex-ctl"},
			RequiredServices:      []string{"containerd.service", "nvidia-fabricmanager.service", "nvidia-imex.service"},
			RequiredDevices:       []string{"/dev/nvidia0", "/dev/nvidia1", "/dev/nvidia2", "/dev/nvidia3", "/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-caps"},
			RequiredKernelModules: []string{"nvidia", "nvidia_drm", "nvidia_modeset", "nvidia_uvm", "mana", "mana_ib", "gdma"},
		},
		Acceptance: NVIDIAQualificationAcceptance{
			ContractVersion: "hillsboro.nvidia-host-capabilities/v1",
			ResultVersion:   "hillsboro-gb200-acceptance/v1",
			Status:          "qualified",
			QualifiedAt:     ptr("2026-10-14T00:00:00Z"),
			Evidence: NVIDIAQualificationEvidence{
				GPUCount:        ptr(4),
				FourGPUDetected: true,
				LocalNVLink: NVIDIAQualificationNVLink{
					Checked:  true,
					Healthy:  true,
					Evidence: ptr("nvidia-smi nvlink --status passed"),
				},
			},
			FailClosedReasons: []string{},
		},
		Integrity: NVIDIAQualificationIntegrity{
			ArtifactPath:     nvidiaIMEXQualificationPath,
			Canonicalization: "RFC8785-JCS",
			Encoding:         "UTF-8-without-BOM",
			DigestAlgorithm:  "sha256",
			DigestSidecar:    "nvidia-gpu-operator.json.sha256",
		},
		Authenticity: NVIDIAQualificationAuthenticity{
			Status:                "verified",
			RequiredBeforeRelease: true,
			Attestation:           ptr("attestation://hillsboro/image/2026.10.14"),
		},
		DocumentDigest: testNVIDIAQualificationDigest,
	}
}

func TestNVIDIAStateAvailable(t *testing.T) {
	t.Parallel()

	require.False(t, NVIDIAStateAvailable(NvidiaHost{}))
	require.True(t, NVIDIAStateAvailable(NvidiaHost{
		GPUDevicePaths: []string{"/dev/nvidia0"},
		LibMappings:    []NvidiaLibMapping{{HostPath: "/host/libcuda.so.1"}},
		DriverVersion:  "580.1",
	}))
}

func TestDiscoverNVIDIADevicesBindsCapabilityDirectories(t *testing.T) {
	t.Parallel()

	deviceDir := t.TempDir()
	capsDir := filepath.Join(deviceDir, nvidiaCapsDirName)
	capability := filepath.Join(capsDir, "nvidia-cap4323")
	imexDir := filepath.Join(deviceDir, nvidiaIMEXDirName)
	channel := filepath.Join(imexDir, "channel0")
	nonChannel := filepath.Join(imexDir, "not-a-channel")

	require.NoError(t, os.MkdirAll(capsDir, 0o755))
	require.NoError(t, os.MkdirAll(imexDir, 0o755))
	require.NoError(t, os.WriteFile(capability, nil, 0o644))
	require.NoError(t, os.WriteFile(channel, nil, 0o644))
	require.NoError(t, os.WriteFile(nonChannel, nil, 0o644))

	devices := discoverNVIDIADevicesIn(deviceDir)
	require.Contains(t, devices, capsDir)
	require.Contains(t, devices, imexDir)
	require.NotContains(t, devices, capability)
	require.NotContains(t, devices, channel)
	require.NotContains(t, devices, nonChannel)
}

func TestNvidiaLocalNVLinkStatusHealthyRequiresEveryGPU(t *testing.T) {
	t.Parallel()

	require.False(t, nvidiaLocalNVLinkStatusHealthy([]byte(
		"GPU 0:\n Link 0: 50 GB/s\n"+
			"GPU 1:\n Link 0: 50 GB/s\n"+
			"GPU 2:\n Link 0: 50 GB/s\n",
	)))
	require.False(t, nvidiaLocalNVLinkStatusHealthy([]byte(
		"GPU 0:\n Link 0: 50 GB/s\n"+
			"GPU 1:\n Link 0: inactive\n"+
			"GPU 2:\n Link 0: 50 GB/s\n"+
			"GPU 3:\n Link 0: 50 GB/s\n",
	)))
	require.True(t, nvidiaLocalNVLinkStatusHealthy([]byte(
		"GPU 0:\n Link 0: 50 GB/s\n"+
			"GPU 1:\n Link 0: 50 GB/s\n"+
			"GPU 2:\n Link 0: 50 GB/s\n"+
			"GPU 3:\n Link 0: 50 GB/s\n",
	)))
}

func TestDiscoverNVIDIABinaryRequiresExecutableRegularFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nvidia-imex")
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	require.Empty(t, discoverNVIDIABinaryIn("nvidia-imex", []string{dir}))

	require.NoError(t, os.Chmod(path, 0o755))
	require.Equal(t, path, discoverNVIDIABinaryIn("nvidia-imex", []string{dir}))

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o755))
	require.Empty(t, discoverNVIDIABinaryIn("nvidia-imex", []string{dir}))
}

func TestResolveNVIDIAIMEXQualification(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nvidia-gpu-operator.json")
	qualification := validTestNVIDIAQualification()
	qualification.DocumentDigest = ""
	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	data, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	require.NoError(t, os.WriteFile(path+".sha256", []byte(digest+"  "+filepath.Base(path)+"\n"), 0o600))

	got, err := resolveNVIDIAIMEXQualification(path)
	require.NoError(t, err)
	require.Equal(t, 1, got.SchemaVersion)
	require.Equal(t, "hillsboro-gb200-gpu-operator-ready", got.Profile)
	require.Equal(t, "580.159.04", got.Driver.Version)
	require.Equal(t, "nvidia-imex-580.159.04-1.azl3.aarch64", *got.IMEX.Package)
	require.True(t, validSHA256Digest(got.DocumentDigest))
}

func TestResolveNVIDIAIMEXQualificationRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	qualification := validTestNVIDIAQualification()
	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	document["unexpected"] = true
	encoded, err = json.Marshal(document)
	require.NoError(t, err)
	canonical, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "qualification.json")
	require.NoError(t, os.WriteFile(path, canonical, 0o600))

	_, err = resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, "unknown field")
}

func TestResolveNVIDIAIMEXQualificationRejectsMissingRequiredFalseField(t *testing.T) {
	t.Parallel()

	qualification := validTestNVIDIAQualification()
	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	imex := document["imex"].(map[string]any)
	service := imex["service"].(map[string]any)
	delete(service, "enabled")
	encoded, err = json.Marshal(document)
	require.NoError(t, err)
	canonical, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "nvidia-gpu-operator.json")
	require.NoError(t, os.WriteFile(path, canonical, 0o600))

	_, err = resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, `required field "imex.service.enabled" is missing`)
}

func TestResolveNVIDIAIMEXQualificationRejectsUnknownNestedField(t *testing.T) {
	t.Parallel()

	qualification := validTestNVIDIAQualification()
	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	imex := document["imex"].(map[string]any)
	service := imex["service"].(map[string]any)
	service["unexpected"] = true
	encoded, err = json.Marshal(document)
	require.NoError(t, err)
	canonical, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "nvidia-gpu-operator.json")
	require.NoError(t, os.WriteFile(path, canonical, 0o600))

	_, err = resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, `unknown field "unexpected"`)
}

func TestResolveNVIDIAIMEXQualificationRejectsNullArray(t *testing.T) {
	t.Parallel()

	qualification := validTestNVIDIAQualification()
	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	acceptance := document["acceptance"].(map[string]any)
	acceptance["failClosedReasons"] = nil
	encoded, err = json.Marshal(document)
	require.NoError(t, err)
	canonical, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "nvidia-gpu-operator.json")
	require.NoError(t, os.WriteFile(path, canonical, 0o600))

	_, err = resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, `required field "acceptance.failClosedReasons" must be an array`)
}

func TestResolveNVIDIAIMEXQualificationRejectsDuplicateFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "qualification.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"schemaVersion":1,"schemaVersion":1}`), 0o600))

	_, err := resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, "duplicate qualification field")
}

func TestResolveNVIDIAIMEXQualificationRejectsNoncanonicalBytes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "qualification.json")
	require.NoError(t, os.WriteFile(path, []byte("{\"schemaVersion\":1}\n"), 0o600))

	_, err := resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, "not RFC 8785 canonical JSON")
}

func TestResolveNVIDIAIMEXQualificationRejectsSidecarMismatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nvidia-gpu-operator.json")
	qualification := validTestNVIDIAQualification()
	qualification.DocumentDigest = ""
	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	data, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	require.NoError(t, os.WriteFile(path+".sha256", []byte(strings.Repeat("0", 64)+"  "+filepath.Base(path)+"\n"), 0o600))

	_, err = resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, "does not match canonical artifact")
}

func TestResolveNVIDIAIMEXQualificationRequiresSidecar(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nvidia-gpu-operator.json")
	qualification := validTestNVIDIAQualification()
	qualification.DocumentDigest = ""
	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	data, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = resolveNVIDIAIMEXQualification(path)
	require.ErrorContains(t, err, "digest sidecar")
}

func TestCanonicalNVIDIAQualificationMatchesProducerBlockedExample(t *testing.T) {
	t.Parallel()

	qualification := validTestNVIDIAQualification()
	qualification.DocumentDigest = ""
	qualification.Image = NVIDIAQualificationImage{}
	qualification.IMEX.Package = nil
	qualification.IMEX.Version = nil
	qualification.Toolkit.Version = nil
	qualification.Toolkit.Ready = false
	qualification.ContainerRuntime.Version = nil
	qualification.ContainerRuntime.CDIEnabled = false
	qualification.Acceptance.Status = "blocked"
	qualification.Acceptance.QualifiedAt = nil
	qualification.Acceptance.Evidence = NVIDIAQualificationEvidence{
		GPUCount: nil,
		LocalNVLink: NVIDIAQualificationNVLink{
			Evidence: nil,
		},
	}
	qualification.Acceptance.FailClosedReasons = []string{
		"Approved nvidia-imex package/version/digest is unavailable.",
		"NVIDIA Container Toolkit 1.18.0 or newer is unavailable from an approved repository.",
		"Immutable image identity, package digests, hardware allowlists, and authenticity attestation are unresolved.",
	}
	qualification.Authenticity.Status = "unavailable"
	qualification.Authenticity.Attestation = nil

	encoded, err := json.Marshal(qualification)
	require.NoError(t, err)
	canonical, err := canonicalQualificationJSON(encoded)
	require.NoError(t, err)
	require.Equal(
		t,
		"d82bdff682685fa4c78bf52b3dd57a7287919606820e02079f2cba33644a1e51",
		fmt.Sprintf("%x", sha256.Sum256(canonical)),
	)
}

func TestConfigureNVIDIAIMEXDRA(t *testing.T) {
	t.Parallel()

	imex := &config.NVIDIAIMEXConfig{
		Mode:               config.NVIDIAIMEXModeDRA,
		HostProfile:        "hillsboro-gb200-gpu-operator-ready",
		HostProfileVersion: "v2026.10.14-1",
		HostProfileDigest:  testNVIDIAQualificationDigest,
	}

	valid := func() NvidiaHost {
		qualification := validTestNVIDIAQualification()

		return NvidiaHost{
			GPUDevicePaths: []string{
				"/dev/nvidia0",
				"/dev/nvidia1",
				"/dev/nvidia2",
				"/dev/nvidia3",
				"/dev/nvidiactl",
				"/dev/nvidia-uvm",
				"/dev/nvidia-caps",
				"/dev/nvidia-caps-imex-channels",
			},
			NvidiaSMIPath:            "/usr/bin/nvidia-smi",
			NvidiaIMEXPath:           "/usr/bin/nvidia-imex",
			NvidiaIMEXCtlPath:        "/usr/bin/nvidia-imex-ctl",
			DriverVersion:            "580.159.04",
			IMEXManagementCapability: true,
			IMEXDeviceClass:          true,
			IMEXServiceUnitState:     "masked",
			HostKernelVersion:        "6.18.48.1-1.azl3.aarch64",
			HostArchitecture:         "arm64",
			HostDistroVersion:        "3.0",
			LocalNVLinkHealthy:       true,
			IMEXQualification:        &qualification,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*NvidiaHost)
		distro  string
		wantErr string
	}{
		{name: "supported", distro: hostDistroAzureLinux3},
		{
			name: "missing binaries",
			mutate: func(host *NvidiaHost) {
				host.NvidiaIMEXPath = ""
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "required binaries",
		},
		{
			name: "missing nvidia-smi",
			mutate: func(host *NvidiaHost) {
				host.NvidiaSMIPath = ""
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "required binaries",
		},
		{
			name: "missing management capability",
			mutate: func(host *NvidiaHost) {
				host.IMEXManagementCapability = false
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "fabric-imex-mgmt",
		},
		{
			name: "missing device class",
			mutate: func(host *NvidiaHost) {
				host.IMEXDeviceClass = false
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "character-device class",
		},
		{
			name: "missing capability directory",
			mutate: func(host *NvidiaHost) {
				host.GPUDevicePaths = []string{
					"/dev/nvidia0",
					"/dev/nvidia1",
					"/dev/nvidia2",
					"/dev/nvidia3",
					"/dev/nvidiactl",
					"/dev/nvidia-uvm",
					"/dev/nvidia-caps-imex-channels",
				}
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "/dev/nvidia-caps",
		},
		{
			name: "missing channel directory",
			mutate: func(host *NvidiaHost) {
				host.GPUDevicePaths = []string{
					"/dev/nvidia0",
					"/dev/nvidia1",
					"/dev/nvidia2",
					"/dev/nvidia3",
					"/dev/nvidiactl",
					"/dev/nvidia-uvm",
					"/dev/nvidia-caps",
				}
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "preprovisioned /dev/nvidia-caps-imex-channels",
		},
		{
			name: "host service active",
			mutate: func(host *NvidiaHost) {
				host.IMEXServiceActive = true
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "service to be inactive",
		},
		{
			name: "host service not masked",
			mutate: func(host *NvidiaHost) {
				host.IMEXServiceUnitState = "disabled"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "service to be masked",
		},
		{
			name: "unsupported driver",
			mutate: func(host *NvidiaHost) {
				host.DriverVersion = "570.172.08"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "running NVIDIA version",
		},
		{
			name: "missing qualification",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification = nil
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "host qualification evidence",
		},
		{
			name: "unsupported qualification schema",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.SchemaVersion = 2
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "schemaVersion 1",
		},
		{
			name: "profile mismatch",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.ProfileVersion = "v2"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "expected hillsboro-gb200-gpu-operator-ready/v2026.10.14-1",
		},
		{
			name: "qualification digest mismatch",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.DocumentDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "qualification digest",
		},
		{
			name: "invalid image digest",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Image.Digest = ptr("sha256:invalid")
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "immutable image ID",
		},
		{
			name:    "distro mismatch",
			distro:  hostDistroUbuntu2404,
			wantErr: "OS/kernel",
		},
		{
			name: "distribution version mismatch",
			mutate: func(host *NvidiaHost) {
				host.HostDistroVersion = "24.04.1"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "OS/kernel",
		},
		{
			name: "architecture mismatch",
			mutate: func(host *NvidiaHost) {
				host.HostArchitecture = "amd64"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "running architecture",
		},
		{
			name: "kernel mismatch",
			mutate: func(host *NvidiaHost) {
				host.HostKernelVersion = "6.18.48.1-1.azl3"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "OS/kernel",
		},
		{
			name: "driver branch mismatch",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Driver.Branch = "570"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "qualification driver",
		},
		{
			name: "driver mismatch",
			mutate: func(host *NvidiaHost) {
				host.DriverVersion = "580.178.04"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "running NVIDIA version",
		},
		{
			name: "userspace mismatch",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Driver.UserspaceVersion = "580.178.04"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "qualification driver",
		},
		{
			name: "IMEX package mismatch",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.IMEX.Package = nil
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "approved IMEX package evidence",
		},
		{
			name: "IMEX version mismatch",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.IMEX.Version = ptr("580.178.04")
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "approved IMEX package evidence",
		},
		{
			name: "unqualified service ownership",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.IMEX.Service.Masked = false
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "service ownership",
		},
		{
			name: "missing toolkit evidence",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Toolkit.Ready = false
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "Container Toolkit 1.18.0",
		},
		{
			name: "CDI disabled",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.ContainerRuntime.CDIEnabled = false
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "containerd 2.0.0",
		},
		{
			name: "unsupported acceptance result version",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Acceptance.ResultVersion = "unsupported"
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "acceptance status is blocked or unsupported",
		},
		{
			name: "failed acceptance test",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Acceptance.Status = "blocked"
				host.IMEXQualification.Acceptance.FailClosedReasons = []string{"blocked"}
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "acceptance status is blocked",
		},
		{
			name: "GPU count mismatch",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Acceptance.Evidence.GPUCount = ptr(8)
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "requires four GPUs",
		},
		{
			name: "runtime local NVLink unhealthy",
			mutate: func(host *NvidiaHost) {
				host.LocalNVLinkHealthy = false
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "healthy verified local NVLink",
		},
		{
			name: "authenticity unavailable",
			mutate: func(host *NvidiaHost) {
				host.IMEXQualification.Authenticity.Status = "unavailable"
				host.IMEXQualification.Authenticity.Attestation = nil
			},
			distro:  hostDistroAzureLinux3,
			wantErr: "authenticity is not verified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			host := valid()
			if tt.mutate != nil {
				tt.mutate(&host)
			}

			got, err := configureNVIDIAIMEX(host, imex, tt.distro)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, config.NVIDIAIMEXModeDRA, got.IMEXMode)
			require.Contains(t, got.GPUDevicePaths, "/dev/nvidia-caps-imex-channels")
		})
	}
}

func TestConfigureNVIDIAIMEXDisabledRemovesExposure(t *testing.T) {
	t.Parallel()

	got, err := configureNVIDIAIMEX(NvidiaHost{
		GPUDevicePaths:    []string{"/dev/nvidia0", "/dev/nvidia-caps-imex-channels"},
		NvidiaIMEXPath:    "/usr/bin/nvidia-imex",
		NvidiaIMEXCtlPath: "/usr/bin/nvidia-imex-ctl",
	}, nil, hostDistroAzureLinux3)
	require.NoError(t, err)
	require.NotContains(t, got.GPUDevicePaths, "/dev/nvidia-caps-imex-channels")
	require.Empty(t, got.NvidiaIMEXPath)
	require.Empty(t, got.NvidiaIMEXCtlPath)
	require.Empty(t, got.IMEXMode)
}

func TestParseNVIDIALibraries(t *testing.T) {
	ldconfigOutput := []byte(`	linux-vdso.so.1 (LINUX_VDSO) => linux-vdso.so.1
	libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
	libcuda.so.1 (libc6) => /usr/lib/i386-linux-gnu/libcuda.so.1
	libnvidia-ml.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1
	libnvidia-ml.so.580.126.09 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.580.126.09
	libEGL_nvidia.so.0 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libEGL_nvidia.so.0
	libGLX_nvidia.so.0 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libGLX_nvidia.so.0
	libGLESv2_nvidia.so.2 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libGLESv2_nvidia.so.2
	libnvcuvid.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvcuvid.so.1
	libnvoptix.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvoptix.so.1
	libpthread.so.0 (libc6,x86-64) => /lib/x86_64-linux-gnu/libpthread.so.0
`)

	libs := parseNVIDIALibraries(ldconfigOutput, "x86-64")

	// Should have 8 NVIDIA libs (the 32-bit libcuda should be filtered out,
	// and non-NVIDIA libs like libpthread should be excluded).
	if got := len(libs); got != 8 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 8", got)
	}

	// Verify first entry.
	if libs[0].HostPath != "/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
		t.Errorf("libs[0].HostPath = %q, want %q", libs[0].HostPath, "/usr/lib/x86_64-linux-gnu/libcuda.so.1")
	}

	// Verify no 32-bit libs.
	for _, lib := range libs {
		if lib.HostPath == "/usr/lib/i386-linux-gnu/libcuda.so.1" {
			t.Errorf("32-bit libcuda should have been filtered out")
		}
	}

	// Verify non-NVIDIA libs are excluded.
	for _, lib := range libs {
		if lib.HostPath == "/lib/x86_64-linux-gnu/libpthread.so.0" {
			t.Errorf("non-NVIDIA library libpthread should have been excluded")
		}
	}
}

func TestExpandNVIDIALibrariesFindsVersionedTargetsMissingFromLDConfig(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	version := "580.167.08"

	for _, family := range []string{"libcuda.so", "libnvidia-ml.so"} {
		versionedName := family + "." + version
		require.NoError(t, os.WriteFile(filepath.Join(libDir, versionedName), []byte(family), 0o644))
		require.NoError(t, os.Symlink(versionedName, filepath.Join(libDir, family+".1")))
	}

	require.NoError(t, os.Symlink("libcuda.so.1", filepath.Join(libDir, "libcuda.so")))
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libcudadebugger.so."+version), []byte("debugger"), 0o644))

	ldconfigOutput := []byte(
		"libcuda.so.1 (libc6,x86-64) => " + filepath.Join(libDir, "libcuda.so.1") + "\n" +
			"libcuda.so (libc6,x86-64) => " + filepath.Join(libDir, "libcuda.so") + "\n" +
			"libnvidia-ml.so.1 (libc6,x86-64) => " + filepath.Join(libDir, "libnvidia-ml.so.1") + "\n",
	)

	libs := expandNVIDIALibraries(parseNVIDIALibraries(ldconfigOutput, "x86-64"))

	paths := make([]string, 0, len(libs))
	for _, lib := range libs {
		paths = append(paths, lib.HostPath)
	}

	require.Contains(t, paths, filepath.Join(libDir, "libcuda.so."+version))
	require.Contains(t, paths, filepath.Join(libDir, "libcudadebugger.so."+version))
	require.Contains(t, paths, filepath.Join(libDir, "libnvidia-ml.so."+version))
	require.Equal(t, version, discoverNVIDIADriverVersion(libs, filepath.Join(t.TempDir(), "missing-version")))
}

func TestExpandNVIDIALibrariesUsesSameBasenameSymlinkTargetAsSource(t *testing.T) {
	t.Parallel()

	aliasDir := t.TempDir()
	targetDir := t.TempDir()
	aliasPath := filepath.Join(aliasDir, "libcuda.so.1")
	targetPath := filepath.Join(targetDir, "libcuda.so.1")

	require.NoError(t, os.WriteFile(targetPath, []byte("cuda"), 0o644))
	require.NoError(t, os.Symlink(targetPath, aliasPath))

	libs := expandNVIDIALibraries([]NvidiaLibMapping{{HostPath: aliasPath}})
	require.Len(t, libs, 1)
	require.Equal(t, targetPath, libs[0].ResolvedHostPath)

	libs, mounts := buildNVIDIALibMounts(libs, "/usr/lib/x86_64-linux-gnu")
	require.Len(t, mounts, 1)
	require.Equal(t, targetDir, mounts[0].HostDir)
	require.Equal(t, "/run/host-nvidia/0/libcuda.so.1", libs[0].ContainerPath)
	require.Equal(t, "/usr/lib/x86_64-linux-gnu/libcuda.so.1", libs[0].LinkPath)
}

func TestDiscoverNVIDIADriverVersionFallsBackToGLX(t *testing.T) {
	t.Parallel()

	libs := []NvidiaLibMapping{{HostPath: "/usr/lib/x86_64-linux-gnu/libGLX_nvidia.so.580.167.08"}}
	require.Equal(t, "580.167.08", discoverNVIDIADriverVersion(libs, filepath.Join(t.TempDir(), "missing-version")))
}

func TestDiscoverNVIDIADriverVersionFallsBackToKernelModule(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	moduleVersionPath := filepath.Join(t.TempDir(), "version")
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libcuda.so.1"), []byte("driver"), 0o644))
	require.NoError(t, os.WriteFile(moduleVersionPath, []byte("580.142\n"), 0o644))

	libs := []NvidiaLibMapping{{HostPath: filepath.Join(libDir, "libcuda.so.1")}}
	require.Equal(t, "580.142", discoverNVIDIADriverVersion(libs, moduleVersionPath))
}

func TestIsNVIDIADriverVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		version string
		valid   bool
	}{
		{version: "580.142", valid: true},
		{version: "580.142.01", valid: true},
		{version: "580", valid: false},
		{version: "580.", valid: false},
		{version: ".142", valid: false},
		{version: "580..142", valid: false},
		{version: "580.142.", valid: false},
		{version: "580.142.01.2", valid: false},
		{version: "580.142-beta", valid: false},
	} {
		t.Run(test.version, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, test.valid, isNVIDIADriverVersion(test.version))
		})
	}
}

func TestParseNVIDIALibraries_ARM64(t *testing.T) {
	ldconfigOutput := []byte(`	linux-vdso.so.1 (LINUX_VDSO) => linux-vdso.so.1
	libcuda.so.1 (libc6,AArch64) => /usr/lib/aarch64-linux-gnu/libcuda.so.1
	libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
	libnvidia-ml.so.1 (libc6,AArch64) => /usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1
	libpthread.so.0 (libc6,AArch64) => /lib/aarch64-linux-gnu/libpthread.so.0
`)

	libs := parseNVIDIALibraries(ldconfigOutput, "aarch64")

	// Should have 2 NVIDIA libs: only aarch64 entries, no x86-64 or non-NVIDIA.
	if got := len(libs); got != 2 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 2", got)
	}

	if libs[0].HostPath != "/usr/lib/aarch64-linux-gnu/libcuda.so.1" {
		t.Errorf("libs[0].HostPath = %q, want aarch64 path", libs[0].HostPath)
	}

	// Verify x86-64 entry is excluded.
	for _, lib := range libs {
		if lib.HostPath == "/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
			t.Errorf("x86-64 libcuda should have been filtered out for aarch64 arch tag")
		}
	}
}

func TestParseNVIDIALibraries_Empty(t *testing.T) {
	libs := parseNVIDIALibraries([]byte(""), "x86-64")
	if len(libs) != 0 {
		t.Errorf("parseNVIDIALibraries(empty) returned %d libs, want 0", len(libs))
	}

	libs = parseNVIDIALibraries([]byte("some garbage output\nno libraries here"), "x86-64")
	if len(libs) != 0 {
		t.Errorf("parseNVIDIALibraries(garbage) returned %d libs, want 0", len(libs))
	}
}

func TestParseNVIDIALibraries_DeduplicatesByBasename(t *testing.T) {
	ldconfigOutput := []byte(`	libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
	libcuda.so.1 (libc6,x86-64) => /usr/lib64/libcuda.so.1
`)

	libs := parseNVIDIALibraries(ldconfigOutput, "x86-64")
	if got := len(libs); got != 1 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 1 (deduped)", got)
	}
	// First match wins.
	if libs[0].HostPath != "/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
		t.Errorf("expected first match to win, got %q", libs[0].HostPath)
	}
}

func TestParseNVIDIALibraries_IncludesVDPAU(t *testing.T) {
	libs := parseNVIDIALibraries([]byte(
		"libvdpau_nvidia.so.580.126.09 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/vdpau/libvdpau_nvidia.so.580.126.09\n",
	), "x86-64")

	if got := len(libs); got != 1 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 1", got)
	}

	if got, want := libs[0].HostPath, "/usr/lib/x86_64-linux-gnu/vdpau/libvdpau_nvidia.so.580.126.09"; got != want {
		t.Errorf("HostPath = %q, want %q", got, want)
	}
}

func TestResolveNVIDIAI386LibrariesScansMatchingDriverDirectory(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	version := "580.126.09"
	files := []string{
		"libGLX_nvidia.so." + version,
		"libGLX_nvidia.so.0",
		"libcudadebugger.so." + version,
		"libGLdispatch.so.0",
		"libOpenCL.so.1",
	}

	for _, name := range files {
		require.NoError(t, os.WriteFile(filepath.Join(libDir, name), []byte(name), 0o644))
	}

	vdpauDir := filepath.Join(libDir, "vdpau")
	vdpauName := "libvdpau_nvidia.so." + version

	require.NoError(t, os.Mkdir(vdpauDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdpauDir, vdpauName), []byte(vdpauName), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libpthread.so.0"), nil, 0o644))

	libs, mounts := resolveNVIDIAI386Libraries(version, []string{libDir})
	libNames := make([]string, 0, len(libs))

	for _, lib := range libs {
		libNames = append(libNames, filepath.Base(lib.HostPath))

		mountIndex := "0"
		if filepath.Base(filepath.Dir(lib.HostPath)) == "vdpau" {
			mountIndex = "1"
		}

		require.Equal(t, filepath.Join(NvidiaHostI386LibDir, mountIndex, filepath.Base(lib.HostPath)), lib.ContainerPath)
	}

	require.ElementsMatch(t, append(files, vdpauName), libNames)
	require.Len(t, mounts, 2)
	require.Equal(t, libDir, mounts[0].HostDir)
	require.Equal(t, "/run/host-nvidia-i386/0", mounts[0].ContainerDir)
	require.Equal(t, vdpauDir, mounts[1].HostDir)
	require.Equal(t, "/run/host-nvidia-i386/1", mounts[1].ContainerDir)
}

func TestResolveNVIDIAI386LibrariesRejectsMismatchedDriver(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libGLX_nvidia.so.570.86.15"), nil, 0o644))

	libs, mounts := resolveNVIDIAI386Libraries("580.126.09", []string{libDir})
	require.Empty(t, libs)
	require.Empty(t, mounts)
}

func Test_buildNVIDIALibMounts(t *testing.T) {
	libs := []NvidiaLibMapping{
		{HostPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.1"},
		{HostPath: "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"},
		{HostPath: "/usr/lib64/libnvoptix.so.1"},
	}

	libs, mounts := buildNVIDIALibMounts(libs, "/usr/lib/x86_64-linux-gnu")

	// Two unique directories.
	if got := len(mounts); got != 2 {
		t.Fatalf("buildNVIDIALibMounts() returned %d mounts, want 2", got)
	}

	// Verify sorted by container path and index matches.
	if mounts[0].ContainerDir != "/run/host-nvidia/0" {
		t.Errorf("mounts[0].ContainerDir = %q, want /run/host-nvidia/0", mounts[0].ContainerDir)
	}

	if mounts[0].Index != 0 {
		t.Errorf("mounts[0].Index = %d, want 0", mounts[0].Index)
	}

	if mounts[1].ContainerDir != "/run/host-nvidia/1" {
		t.Errorf("mounts[1].ContainerDir = %q, want /run/host-nvidia/1", mounts[1].ContainerDir)
	}

	if mounts[1].Index != 1 {
		t.Errorf("mounts[1].Index = %d, want 1", mounts[1].Index)
	}

	// Verify container paths are stamped on library mappings.
	// /usr/lib/x86_64-linux-gnu sorts before /usr/lib64 ('/' < '6'), so:
	//   index 0 = /usr/lib/x86_64-linux-gnu -> /run/host-nvidia/0
	//   index 1 = /usr/lib64 -> /run/host-nvidia/1
	if libs[0].ContainerPath != "/run/host-nvidia/0/libcuda.so.1" {
		t.Errorf("libs[0].ContainerPath = %q, want /run/host-nvidia/0/libcuda.so.1", libs[0].ContainerPath)
	}

	if libs[2].ContainerPath != "/run/host-nvidia/1/libnvoptix.so.1" {
		t.Errorf("libs[2].ContainerPath = %q, want /run/host-nvidia/1/libnvoptix.so.1", libs[2].ContainerPath)
	}
}

func Test_buildNVIDIALibMounts_SingleDirectory(t *testing.T) {
	libs := []NvidiaLibMapping{
		{HostPath: "/usr/lib64/libcuda.so.1"},
		{HostPath: "/usr/lib64/libnvidia-ml.so"},
	}

	libs, mounts := buildNVIDIALibMounts(libs, "/usr/lib/x86_64-linux-gnu")
	if got := len(mounts); got != 1 {
		t.Fatalf("buildNVIDIALibMounts() returned %d mounts, want 1", got)
	}

	// All libs should point into the same mount.
	for i, lib := range libs {
		if lib.ContainerPath == "" {
			t.Errorf("libs[%d].ContainerPath is empty", i)
		}
	}

	if libs[0].ContainerPath != "/run/host-nvidia/0/libcuda.so.1" {
		t.Errorf("libs[0].ContainerPath = %q, want /run/host-nvidia/0/libcuda.so.1", libs[0].ContainerPath)
	}
}

func Test_buildNVIDIALibMounts_Empty(t *testing.T) {
	libs, mounts := buildNVIDIALibMounts(nil, "/usr/lib/x86_64-linux-gnu")
	if libs != nil {
		t.Errorf("buildNVIDIALibMounts(nil) returned non-nil libs")
	}

	if mounts != nil {
		t.Errorf("buildNVIDIALibMounts(nil) returned non-nil mounts")
	}
}
