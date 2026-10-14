// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/config"
)

// NVIDIA host discovery and goal state resolution.
//
// When the NVIDIA driver is installed on the host but the nspawn container
// uses a separate rootfs (e.g. an OCI image), the driver's userspace
// libraries (libnvidia-ml, libcuda, etc.) are only present on the host.
// Tools that need them inside the container, in particular nvidia-ctk for
// CDI spec generation, will fail with "ERROR_LIBRARY_NOT_FOUND".
//
// The library discovery and bind-mount approach is derived from the intuneme
// project (https://github.com/frostyard/intuneme).
//
//  1. Parse `ldconfig -p` on the host to find NVIDIA library directories for
//     the host architecture (x86-64 or aarch64), then scan those directories
//     for aliases and real versioned files.
//  2. Bind-mount the host directories containing those libraries into the
//     nspawn container at /run/host-nvidia/0/, /run/host-nvidia/1/, etc.
//  3. After the nspawn boots, create symlinks in the container's standard
//     multiarch library path pointing into the bind mounts and run ldconfig
//     to update the linker cache.

// ErrNVIDIAStateUnavailable indicates that discovered NVIDIA devices do not
// have the complete host state needed for safe startup.
var ErrNVIDIAStateUnavailable = errors.New("required NVIDIA host state is unavailable")

// ErrNVIDIAHostUnqualified indicates that a configured prequalified NVIDIA
// host failed the immutable evidence or runtime prerequisite contract.
var ErrNVIDIAHostUnqualified = errors.New("NVIDIA host qualification failed")

// NvidiaHost aggregates all NVIDIA-related host state discovered at agent
// startup: GPU device paths, driver library mappings, and the derived
// bind-mount specifications for the nspawn container.
type NvidiaHost struct {
	// Required records whether NVIDIA capability was selected when the machine
	// was provisioned. Discovery refreshes must preserve this value.
	Required bool

	// GPUDevicePaths lists NVIDIA GPU device paths discovered on the host
	// (e.g. /dev/nvidia0, /dev/nvidiactl, /dev/nvidia-caps,
	// /dev/nvidia-caps-imex-channels, /dev/dri/*).
	// When non-empty the nspawn configuration will bind-mount these devices
	// and grant the container cgroup access to them.
	GPUDevicePaths []string

	// ContainerLibDir is the architecture-specific multiarch library
	// directory inside the nspawn container (e.g. /usr/lib/x86_64-linux-gnu
	// on amd64, /usr/lib/aarch64-linux-gnu on arm64). Symlinks to
	// bind-mounted host NVIDIA libraries are created here.
	ContainerLibDir string

	// LibMappings contains NVIDIA userspace libraries discovered from
	// ldconfig -p and filesystem scans. These are used to create symlinks
	// inside the nspawn container so that the host's NVIDIA driver libraries
	// are accessible.
	LibMappings []NvidiaLibMapping

	// LibDirMounts lists unique host directories containing NVIDIA libraries
	// to be bind-mounted read-only into the nspawn container at
	// /run/host-nvidia/<index>/. After boot, symlinks from the container's
	// standard library path are created by the setup-nvidia-libraries task.
	LibDirMounts []NvidiaLibDirMount

	// I386LibMappings contains optional i386 NVIDIA and GLVND libraries with a
	// driver version matching the active 64-bit driver.
	I386LibMappings []NvidiaLibMapping

	// I386LibDirMounts lists the host directories for I386LibMappings.
	I386LibDirMounts []NvidiaLibDirMount

	// NvidiaSMIPath is the host path to nvidia-smi, when available.
	NvidiaSMIPath string

	// NvidiaIMEXPath is the host path to nvidia-imex, when available.
	NvidiaIMEXPath string

	// NvidiaIMEXCtlPath is the host path to nvidia-imex-ctl, when available.
	NvidiaIMEXCtlPath string

	// DriverVersion is the active NVIDIA kernel driver version. It is used to
	// provide versioned library names when a host installation exposes only
	// unversioned or SONAME aliases through ldconfig.
	DriverVersion string

	// IMEXMode records the selected IMEX lifecycle owner. Empty means IMEX
	// preparation and exposure are disabled.
	IMEXMode config.NVIDIAIMEXMode

	// IMEXManagementCapability reports whether the loaded driver exposes the
	// fabric IMEX management capability required by nvidia-imex.
	IMEXManagementCapability bool

	// IMEXDeviceClass reports whether the kernel registered the
	// nvidia-caps-imex-channels character-device class.
	IMEXDeviceClass bool

	// IMEXServiceActive is true when the host nvidia-imex service is running.
	IMEXServiceActive bool

	// IMEXServiceUnitState is the systemd unit-file state, such as masked.
	IMEXServiceUnitState string

	// IMEXQualification records immutable host-image evidence for the selected
	// GPU Operator readiness profile.
	IMEXQualification *NVIDIAIMEXQualification

	// HostKernelVersion is the running host kernel release.
	HostKernelVersion string

	// HostArchitecture is the running host GOARCH value.
	HostArchitecture string

	// HostDistroVersion is the running host VERSION_ID from /etc/os-release.
	HostDistroVersion string

	// LocalNVLinkHealthy reports whether nvidia-smi found active local links
	// without any inactive link in its read-only status output.
	LocalNVLinkHealthy bool
}

// NVIDIAIMEXQualification is written by the host-image provisioning pipeline.
// The agent treats it as evidence only and also verifies the corresponding
// runtime driver, binaries, devices, and service state.
type NVIDIAIMEXQualification struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Profile         string `json:"profile"`
	ProfileVersion  string `json:"profileVersion"`
	ContractVersion string `json:"contractVersion"`
	Architecture    string `json:"architecture"`

	Image            NVIDIAQualificationImage            `json:"image"`
	Distro           NVIDIAQualificationDistro           `json:"distro"`
	Driver           NVIDIAQualificationPackage          `json:"driver"`
	FabricManager    NVIDIAQualificationPackage          `json:"fabricManager"`
	NSCQ             NVIDIAQualificationPackage          `json:"nscq"`
	IMEX             NVIDIAQualificationIMEX             `json:"imex"`
	Toolkit          NVIDIAQualificationToolkit          `json:"toolkit"`
	ContainerRuntime NVIDIAQualificationContainerRuntime `json:"containerRuntime"`
	Prerequisites    NVIDIAQualificationPrerequisites    `json:"prerequisites"`
	Acceptance       NVIDIAQualificationAcceptance       `json:"acceptance"`
	Integrity        NVIDIAQualificationIntegrity        `json:"integrity"`
	Authenticity     NVIDIAQualificationAuthenticity     `json:"authenticity"`

	DocumentDigest string `json:"-"`
}

type NVIDIAQualificationImage struct {
	ID      *string `json:"id"`
	Version *string `json:"version"`
	Digest  *string `json:"digest"`
}

type NVIDIAQualificationDistro struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Kernel      string `json:"kernel"`
	KernelDevel string `json:"kernelDevel"`
}

type NVIDIAQualificationPackage struct {
	Branch           string `json:"branch,omitempty"`
	Version          string `json:"version"`
	UserspaceVersion string `json:"userspaceVersion,omitempty"`
	Package          string `json:"package"`
}

type NVIDIAQualificationIMEX struct {
	Package        *string                    `json:"package"`
	Version        *string                    `json:"version"`
	ExpectedPaths  []string                   `json:"expectedPaths"`
	Service        NVIDIAQualificationService `json:"service"`
	LifecycleOwner string                     `json:"lifecycleOwner"`
}

type NVIDIAQualificationService struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Masked  bool   `json:"masked"`
}

type NVIDIAQualificationToolkit struct {
	Version        *string `json:"version"`
	MinimumVersion string  `json:"minimumVersion"`
	Ready          bool    `json:"ready"`
}

type NVIDIAQualificationContainerRuntime struct {
	Name               string   `json:"name"`
	Version            *string  `json:"version"`
	MinimumVersion     string   `json:"minimumVersion"`
	CDIEnabled         bool     `json:"cdiEnabled"`
	CDISpecDirectories []string `json:"cdiSpecDirectories"`
}

type NVIDIAQualificationPrerequisites struct {
	RequiredFiles         []string `json:"requiredFiles"`
	RequiredServices      []string `json:"requiredServices"`
	RequiredDevices       []string `json:"requiredDevices"`
	RequiredKernelModules []string `json:"requiredKernelModules"`
}

type NVIDIAQualificationAcceptance struct {
	ContractVersion   string                      `json:"contractVersion"`
	ResultVersion     string                      `json:"resultVersion"`
	Status            string                      `json:"status"`
	QualifiedAt       *string                     `json:"qualifiedAt"`
	Evidence          NVIDIAQualificationEvidence `json:"evidence"`
	FailClosedReasons []string                    `json:"failClosedReasons"`
}

type NVIDIAQualificationEvidence struct {
	GPUCount               *int                      `json:"gpuCount"`
	FourGPUDetected        bool                      `json:"fourGpuDetected"`
	LocalNVLink            NVIDIAQualificationNVLink `json:"localNvlink"`
	MultiNodeNVLinkClaimed bool                      `json:"multiNodeNvlinkClaimed"`
}

type NVIDIAQualificationNVLink struct {
	Checked  bool    `json:"checked"`
	Healthy  bool    `json:"healthy"`
	Evidence *string `json:"evidence"`
}

type NVIDIAQualificationIntegrity struct {
	ArtifactPath     string `json:"artifactPath"`
	Canonicalization string `json:"canonicalization"`
	Encoding         string `json:"encoding"`
	TrailingNewline  bool   `json:"trailingNewline"`
	DigestAlgorithm  string `json:"digestAlgorithm"`
	DigestSidecar    string `json:"digestSidecar"`
}

type NVIDIAQualificationAuthenticity struct {
	Status                string  `json:"status"`
	RequiredBeforeRelease bool    `json:"requiredBeforeRelease"`
	Attestation           *string `json:"attestation"`
}

// NvidiaLibMapping maps a host NVIDIA library to its corresponding paths
// inside the nspawn container.
type NvidiaLibMapping struct {
	HostPath         string // e.g. "/usr/lib/x86_64-linux-gnu/libcuda.so.1", original discovered name
	ResolvedHostPath string // e.g. "/usr/lib/nvidia/libcuda.so.580.126.09", real bind-mount source
	ContainerPath    string // e.g. "/run/host-nvidia/0/libcuda.so.580.126.09", bind-mount source
	LinkPath         string // e.g. "/usr/lib/x86_64-linux-gnu/libcuda.so.1", symlink in container
}

// NvidiaLibDirMount represents a read-only bind mount of a host directory
// containing NVIDIA libraries into the nspawn container.
type NvidiaLibDirMount struct {
	Index        int    // mount index, used by symlink creation to map libs to their container path
	HostDir      string // e.g. "/usr/lib/x86_64-linux-gnu"
	ContainerDir string // e.g. "/run/host-nvidia/0"
}

// NvidiaRuntime describes the NVIDIA container runtime configuration for
// containerd. When Enabled is true the runtime is registered as a handler
// so that GPU workloads can be scheduled.
type NvidiaRuntime struct {
	Enabled                    bool
	RuntimeClassName           string
	RuntimePath                string
	DisableSetAsDefaultRuntime bool
}

// ResolveNvidiaHost probes the host for NVIDIA GPU devices and driver
// libraries, returning a fully populated NvidiaHost. The arch parameter is a
// GOARCH value (e.g. "amd64", "arm64") used to select the correct multiarch
// library path and ldconfig filter. Returns an error for unsupported
// architectures. On a non-GPU host the returned struct has all nil/empty
// fields (except ContainerLibDir).
func ResolveNvidiaHost(arch string) (NvidiaHost, error) {
	archInfo, ok := nvidiaArchMap[arch]
	if !ok {
		return NvidiaHost{}, fmt.Errorf("unsupported architecture %q for NVIDIA library discovery", arch)
	}

	devices := discoverNVIDIADevices()
	libraries := resolveNVIDIALibraries(archInfo)
	nvidiaSMIPath := discoverNVIDIASMI()
	imexPath := discoverNVIDIABinary("nvidia-imex")
	imexCtlPath := discoverNVIDIABinary("nvidia-imex-ctl")

	var (
		imexServiceActive    bool
		imexServiceUnitState string
	)

	if imexPath != "" || imexCtlPath != "" {
		imexServiceActive = nvidiaIMEXServiceActive()
		imexServiceUnitState = nvidiaIMEXServiceUnitState()
	}

	kernelVersion, err := hostKernel()
	if err != nil {
		return NvidiaHost{}, fmt.Errorf("inspect NVIDIA host kernel: %w", err)
	}

	return NvidiaHost{
		GPUDevicePaths:    devices,
		ContainerLibDir:   archInfo.libDir,
		LibMappings:       libraries.libMappings,
		LibDirMounts:      libraries.libDirMounts,
		I386LibMappings:   libraries.i386LibMappings,
		I386LibDirMounts:  libraries.i386LibDirMounts,
		NvidiaSMIPath:     nvidiaSMIPath,
		NvidiaIMEXPath:    imexPath,
		NvidiaIMEXCtlPath: imexCtlPath,
		DriverVersion:     libraries.driverVersion,
		IMEXManagementCapability: pathExists(
			"/proc/driver/nvidia/capabilities/fabric-imex-mgmt",
		),
		IMEXDeviceClass:      procDeviceClassExists("/proc/devices", nvidiaIMEXDirName),
		IMEXServiceActive:    imexServiceActive,
		IMEXServiceUnitState: imexServiceUnitState,
		HostKernelVersion:    kernelVersion,
		HostArchitecture:     arch,
		HostDistroVersion:    hostDistroVersionFromOSRelease(hostOSReleasePath),
		LocalNVLinkHealthy:   nvidiaLocalNVLinkHealthy(nvidiaSMIPath),
	}, nil
}

// NVIDIAStateAvailable reports whether discovery produced all host state needed
// to render mounts and perform in-machine setup.
func NVIDIAStateAvailable(nvidia NvidiaHost) bool {
	return len(nvidia.GPUDevicePaths) > 0 && len(nvidia.LibMappings) > 0 && nvidia.DriverVersion != ""
}

// resolveNvidiaRuntime returns the NVIDIA container runtime goal state using
// the caller-selected provisioned capability.
func resolveNvidiaRuntime(enabled bool) NvidiaRuntime {
	return NvidiaRuntime{
		Enabled:                    enabled,
		RuntimeClassName:           NvidiaRuntimeClassName,
		RuntimePath:                NvidiaContainerRuntimePath,
		DisableSetAsDefaultRuntime: false,
	}
}

const (
	devDir            = "/dev"
	nvidiaCapsDirName = "nvidia-caps"
	nvidiaIMEXDirName = "nvidia-caps-imex-channels"
	driDir            = "/dev/dri"
	nvidiaDevPrefix   = "nvidia"

	nvidiaIMEXQualificationPath = "/etc/unbounded/host-capabilities/nvidia-gpu-operator.json"
)

func resolveNVIDIAIMEXQualification(path string) (*NVIDIAIMEXQualification, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read NVIDIA host qualification %s: %w", path, err)
	}

	canonical, err := canonicalQualificationJSON(data)
	if err != nil {
		return nil, fmt.Errorf("canonicalize NVIDIA host qualification %s: %w", path, err)
	}

	if !bytes.Equal(data, canonical) {
		return nil, fmt.Errorf(
			"NVIDIA host qualification %s is not RFC 8785 canonical JSON",
			path,
		)
	}

	if err := validateRequiredNVIDIAQualificationFields(canonical); err != nil {
		return nil, fmt.Errorf("decode NVIDIA host qualification %s: %w", path, err)
	}

	var qualification NVIDIAIMEXQualification

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&qualification); err != nil {
		return nil, fmt.Errorf("decode NVIDIA host qualification %s: %w", path, err)
	}

	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode NVIDIA host qualification %s: %w", path, err)
	}

	digest := fmt.Sprintf("%x", sha256.Sum256(canonical))
	sidecarPath := path + ".sha256"

	sidecar, err := os.ReadFile(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("read NVIDIA host qualification digest sidecar %s: %w", sidecarPath, err)
	}

	expectedSidecar := digest + "  " + filepath.Base(path) + "\n"
	if string(sidecar) != expectedSidecar {
		return nil, fmt.Errorf("NVIDIA host qualification digest sidecar %s does not match canonical artifact", sidecarPath)
	}

	qualification.DocumentDigest = "sha256:" + digest

	return &qualification, nil
}

func canonicalQualificationJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	value, err := parseCanonicalJSONValue(decoder)
	if err != nil {
		return nil, err
	}

	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("qualification must be a JSON object")
	}

	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}

	var canonical bytes.Buffer

	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(value); err != nil {
		return nil, err
	}

	result := bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))
	result = bytes.ReplaceAll(result, []byte(`\u2028`), []byte("\u2028"))
	result = bytes.ReplaceAll(result, []byte(`\u2029`), []byte("\u2029"))

	return result, nil
}

func validateRequiredNVIDIAQualificationFields(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("decode qualification object: %w", err)
	}

	if err := requireJSONFields(root, "qualification", []string{
		"schemaVersion", "profile", "profileVersion", "contractVersion", "image",
		"architecture", "distro", "driver", "fabricManager", "nscq", "imex",
		"toolkit", "containerRuntime", "prerequisites", "acceptance", "integrity",
		"authenticity",
	}); err != nil {
		return err
	}

	image, err := requiredJSONObject(root, "image", "image")
	if err != nil {
		return err
	}

	if err := requireJSONFields(image, "image", []string{"id", "version", "digest"}); err != nil {
		return err
	}

	distro, err := requiredJSONObject(root, "distro", "distro")
	if err != nil {
		return err
	}

	if err := requireJSONFields(distro, "distro", []string{"name", "version", "kernel", "kernelDevel"}); err != nil {
		return err
	}

	driver, err := requiredJSONObject(root, "driver", "driver")
	if err != nil {
		return err
	}

	if err := requireJSONFields(driver, "driver", []string{"branch", "version", "userspaceVersion", "package"}); err != nil {
		return err
	}

	for _, name := range []string{"fabricManager", "nscq"} {
		value, err := requiredJSONObject(root, name, name)
		if err != nil {
			return err
		}

		if err := requireJSONFields(value, name, []string{"package", "version"}); err != nil {
			return err
		}
	}

	imex, err := requiredJSONObject(root, "imex", "imex")
	if err != nil {
		return err
	}

	if err := requireJSONFields(imex, "imex", []string{"package", "version", "expectedPaths", "service", "lifecycleOwner"}); err != nil {
		return err
	}

	if err := requireJSONArray(imex, "expectedPaths", "imex.expectedPaths"); err != nil {
		return err
	}

	service, err := requiredJSONObject(imex, "service", "imex.service")
	if err != nil {
		return err
	}

	if err := requireJSONFields(service, "imex.service", []string{"name", "enabled", "masked"}); err != nil {
		return err
	}

	if err := requireJSONNonNull(service, "enabled", "imex.service.enabled"); err != nil {
		return err
	}

	toolkit, err := requiredJSONObject(root, "toolkit", "toolkit")
	if err != nil {
		return err
	}

	if err := requireJSONFields(toolkit, "toolkit", []string{"version", "minimumVersion", "ready"}); err != nil {
		return err
	}

	containerRuntime, err := requiredJSONObject(root, "containerRuntime", "containerRuntime")
	if err != nil {
		return err
	}

	if err := requireJSONFields(containerRuntime, "containerRuntime", []string{
		"name", "version", "minimumVersion", "cdiEnabled", "cdiSpecDirectories",
	}); err != nil {
		return err
	}

	if err := requireJSONArray(containerRuntime, "cdiSpecDirectories", "containerRuntime.cdiSpecDirectories"); err != nil {
		return err
	}

	prerequisites, err := requiredJSONObject(root, "prerequisites", "prerequisites")
	if err != nil {
		return err
	}

	if err := requireJSONFields(prerequisites, "prerequisites", []string{
		"requiredFiles", "requiredServices", "requiredDevices", "requiredKernelModules",
	}); err != nil {
		return err
	}

	for _, field := range []string{"requiredFiles", "requiredServices", "requiredDevices", "requiredKernelModules"} {
		if err := requireJSONArray(prerequisites, field, "prerequisites."+field); err != nil {
			return err
		}
	}

	acceptance, err := requiredJSONObject(root, "acceptance", "acceptance")
	if err != nil {
		return err
	}

	if err := requireJSONFields(acceptance, "acceptance", []string{
		"contractVersion", "resultVersion", "status", "qualifiedAt", "evidence", "failClosedReasons",
	}); err != nil {
		return err
	}

	if err := requireJSONArray(acceptance, "failClosedReasons", "acceptance.failClosedReasons"); err != nil {
		return err
	}

	evidence, err := requiredJSONObject(acceptance, "evidence", "acceptance.evidence")
	if err != nil {
		return err
	}

	if err := requireJSONFields(evidence, "acceptance.evidence", []string{
		"gpuCount", "fourGpuDetected", "localNvlink", "multiNodeNvlinkClaimed",
	}); err != nil {
		return err
	}

	if err := requireJSONNonNull(evidence, "multiNodeNvlinkClaimed", "acceptance.evidence.multiNodeNvlinkClaimed"); err != nil {
		return err
	}

	localNVLink, err := requiredJSONObject(evidence, "localNvlink", "acceptance.evidence.localNvlink")
	if err != nil {
		return err
	}

	if err := requireJSONFields(localNVLink, "acceptance.evidence.localNvlink", []string{"checked", "healthy", "evidence"}); err != nil {
		return err
	}

	integrity, err := requiredJSONObject(root, "integrity", "integrity")
	if err != nil {
		return err
	}

	if err := requireJSONFields(integrity, "integrity", []string{
		"artifactPath", "canonicalization", "encoding", "trailingNewline", "digestAlgorithm", "digestSidecar",
	}); err != nil {
		return err
	}

	if err := requireJSONNonNull(integrity, "trailingNewline", "integrity.trailingNewline"); err != nil {
		return err
	}

	authenticity, err := requiredJSONObject(root, "authenticity", "authenticity")
	if err != nil {
		return err
	}

	return requireJSONFields(authenticity, "authenticity", []string{"status", "requiredBeforeRelease", "attestation"})
}

func requiredJSONObject(parent map[string]json.RawMessage, key string, path string) (map[string]json.RawMessage, error) {
	raw, ok := parent[key]
	if !ok {
		return nil, fmt.Errorf("required field %q is missing", path)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("required field %q must be an object", path)
	}

	return object, nil
}

func requireJSONFields(object map[string]json.RawMessage, path string, fields []string) error {
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
		if _, ok := object[field]; !ok {
			return fmt.Errorf("required field %q is missing", path+"."+field)
		}
	}

	for field := range object {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("unknown field %q in %s", field, path)
		}
	}

	return nil
}

func requireJSONArray(parent map[string]json.RawMessage, key string, path string) error {
	raw, ok := parent[key]
	if !ok {
		return fmt.Errorf("required field %q is missing", path)
	}

	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil || array == nil {
		return fmt.Errorf("required field %q must be an array", path)
	}

	return nil
}

func requireJSONNonNull(parent map[string]json.RawMessage, key string, path string) error {
	raw, ok := parent[key]
	if !ok {
		return fmt.Errorf("required field %q is missing", path)
	}

	if bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("required field %q must not be null", path)
	}

	return nil
}

func parseCanonicalJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}

	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		if number, ok := token.(json.Number); ok {
			text := number.String()
			if strings.ContainsAny(text, ".eE") {
				return nil, fmt.Errorf("qualification numbers must be integers")
			}

			value, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("qualification integer %q: %w", text, err)
			}

			const maxExactJSONInteger = int64(1<<53 - 1)
			if value > maxExactJSONInteger || value < -maxExactJSONInteger {
				return nil, fmt.Errorf("qualification integer %q exceeds the exact JSON range", text)
			}

			return value, nil
		}

		return token, nil
	}

	switch delimiter {
	case '{':
		object := make(map[string]any)

		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}

			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("qualification field name must be a string")
			}

			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate qualification field %q", key)
			}

			value, err := parseCanonicalJSONValue(decoder)
			if err != nil {
				return nil, err
			}

			object[key] = value
		}

		end, err := decoder.Token()
		if err != nil {
			return nil, err
		}

		if end != json.Delim('}') {
			return nil, fmt.Errorf("qualification JSON object is not closed")
		}

		return object, nil
	case '[':
		array := make([]any, 0)

		for decoder.More() {
			value, err := parseCanonicalJSONValue(decoder)
			if err != nil {
				return nil, err
			}

			array = append(array, value)
		}

		end, err := decoder.Token()
		if err != nil {
			return nil, err
		}

		if end != json.Delim(']') {
			return nil, fmt.Errorf("qualification JSON array is not closed")
		}

		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}

		return err
	}

	return nil
}

func pathExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func procDeviceClassExists(path, name string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return true
		}
	}

	return false
}

func nvidiaIMEXServiceActive() bool {
	out, err := nvidiaSystemctlShow("ActiveState")
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func nvidiaIMEXServiceUnitState() string {
	out, err := nvidiaSystemctlShow("UnitFileState")
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

func nvidiaLocalNVLinkHealthy(nvidiaSMIPath string) bool {
	if nvidiaSMIPath == "" {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, nvidiaSMIPath, "nvlink", "--status").Output()
	if err != nil {
		return false
	}

	return nvidiaLocalNVLinkStatusHealthy(out)
}

func nvidiaLocalNVLinkStatusHealthy(out []byte) bool {
	const expectedGPUCount = 4

	currentGPU := -1
	linkCounts := make(map[int]int, expectedGPUCount)

	for _, line := range strings.Split(strings.ToLower(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gpu ") {
			indexText, _, ok := strings.Cut(strings.TrimPrefix(line, "gpu "), ":")

			index, err := strconv.Atoi(strings.TrimSpace(indexText))
			if !ok || err != nil || index < 0 || index >= expectedGPUCount {
				currentGPU = -1
				continue
			}

			currentGPU = index
			if _, ok := linkCounts[index]; !ok {
				linkCounts[index] = 0
			}

			continue
		}

		if currentGPU >= 0 && strings.HasPrefix(line, "link ") {
			if strings.Contains(line, "inactive") {
				return false
			}

			linkCounts[currentGPU]++
		}
	}

	if len(linkCounts) != expectedGPUCount {
		return false
	}

	for gpu := range expectedGPUCount {
		if linkCounts[gpu] == 0 {
			return false
		}
	}

	return true
}

func nvidiaSystemctlShow(property string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return exec.CommandContext(
		ctx,
		"systemctl",
		"show",
		"nvidia-imex.service",
		"--property="+property,
		"--value",
	).Output()
}

func configureNVIDIAIMEX(
	nvidia NvidiaHost,
	imex *config.NVIDIAIMEXConfig,
	hostDistro string,
) (NvidiaHost, error) {
	imexDir := filepath.Join(devDir, nvidiaIMEXDirName)
	imexDirPresent := slices.Contains(nvidia.GPUDevicePaths, imexDir)
	nvidia.GPUDevicePaths = slices.DeleteFunc(nvidia.GPUDevicePaths, func(path string) bool {
		return path == imexDir
	})

	if imex == nil || imex.Mode == "" {
		nvidia.NvidiaIMEXPath = ""
		nvidia.NvidiaIMEXCtlPath = ""
		nvidia.IMEXManagementCapability = false
		nvidia.IMEXDeviceClass = false
		nvidia.IMEXServiceActive = false
		nvidia.IMEXServiceUnitState = ""

		return nvidia, nil
	}

	if imex.Mode != config.NVIDIAIMEXModeDRA {
		return NvidiaHost{}, fmt.Errorf("unsupported NVIDIA IMEX mode %q", imex.Mode)
	}

	qualification := nvidia.IMEXQualification
	if qualification == nil {
		return NvidiaHost{}, fmt.Errorf(
			"NVIDIA IMEX DRA mode requires host qualification evidence at %s",
			nvidiaIMEXQualificationPath,
		)
	}

	if qualification.SchemaVersion != 1 {
		return NvidiaHost{}, fmt.Errorf(
			"NVIDIA IMEX DRA mode requires host qualification schemaVersion 1, found %d",
			qualification.SchemaVersion,
		)
	}

	if qualification.Profile != imex.HostProfile ||
		qualification.ProfileVersion != imex.HostProfileVersion {
		return NvidiaHost{}, fmt.Errorf(
			"NVIDIA IMEX DRA host qualification is %s/%s, expected %s/%s",
			qualification.Profile,
			qualification.ProfileVersion,
			imex.HostProfile,
			imex.HostProfileVersion,
		)
	}

	if qualification.DocumentDigest != imex.HostProfileDigest {
		return NvidiaHost{}, fmt.Errorf(
			"NVIDIA IMEX DRA host qualification digest is %q, expected %q",
			qualification.DocumentDigest,
			imex.HostProfileDigest,
		)
	}

	if err := validateNVIDIAQualificationContract(qualification, nvidia, hostDistro); err != nil {
		return NvidiaHost{}, err
	}

	if len(nvidia.GPUDevicePaths) == 0 {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires NVIDIA GPU devices")
	}

	driverMajor, err := nvidiaDriverMajor(nvidia.DriverVersion)
	if err != nil {
		return NvidiaHost{}, err
	}

	if driverMajor < 580 {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires driver 580 or later, found %q", nvidia.DriverVersion)
	}

	if nvidia.NvidiaIMEXPath == "" || nvidia.NvidiaIMEXCtlPath == "" {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires nvidia-imex and nvidia-imex-ctl from the branch-matched host package")
	}

	if !nvidia.IMEXManagementCapability {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires /proc/driver/nvidia/capabilities/fabric-imex-mgmt")
	}

	if !nvidia.IMEXDeviceClass {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires the nvidia-caps-imex-channels character-device class")
	}

	if nvidia.IMEXServiceActive {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires the host nvidia-imex service to be inactive")
	}

	if nvidia.IMEXServiceUnitState != "masked" {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires the host nvidia-imex service to be masked, found %q", nvidia.IMEXServiceUnitState)
	}

	if !slices.Contains(nvidia.GPUDevicePaths, filepath.Join(devDir, nvidiaCapsDirName)) {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires %s", filepath.Join(devDir, nvidiaCapsDirName))
	}

	if !imexDirPresent {
		return NvidiaHost{}, fmt.Errorf("NVIDIA IMEX DRA mode requires preprovisioned %s", imexDir)
	}

	nvidia.IMEXMode = imex.Mode
	nvidia.GPUDevicePaths = append(nvidia.GPUDevicePaths, imexDir)
	slices.Sort(nvidia.GPUDevicePaths)

	return nvidia, nil
}

func validateNVIDIAQualificationContract(
	qualification *NVIDIAIMEXQualification,
	nvidia NvidiaHost,
	hostDistro string,
) error {
	const (
		profileName          = "hillsboro-gb200-gpu-operator-ready"
		contractVersion      = "hillsboro.nvidia-host-capabilities/v1"
		resultVersion        = "hillsboro-gb200-acceptance/v1"
		driverVersion        = "580.159.04"
		driverPackage        = "cuda-open-hwe-580.159.04-2_6.18.48.1.1.azl3.aarch64"
		fabricManagerPackage = "nvidia-fabric-manager-580.159.04-1.azl3.aarch64"
		nscqPackage          = "libnvidia-nscq-580.159.04-1.azl3.aarch64"
		kernelVersion        = "6.18.48.1-1.azl3.aarch64"
	)

	if qualification.SchemaVersion != 1 ||
		qualification.Profile != profileName ||
		qualification.ContractVersion != contractVersion {
		return fmt.Errorf("NVIDIA IMEX DRA qualification schema, profile, or contract version is unsupported")
	}

	if qualification.Architecture != "arm64" ||
		nvidia.HostArchitecture != qualification.Architecture {
		return fmt.Errorf(
			"NVIDIA IMEX DRA host qualification architecture is %q, running architecture is %q",
			qualification.Architecture,
			nvidia.HostArchitecture,
		)
	}

	if qualification.Image.ID == nil ||
		qualification.Image.Version == nil ||
		qualification.Image.Digest == nil ||
		strings.TrimSpace(*qualification.Image.ID) == "" ||
		strings.TrimSpace(*qualification.Image.Version) == "" ||
		!validSHA256Digest(*qualification.Image.Digest) {
		return fmt.Errorf("NVIDIA IMEX DRA qualification must record a verified immutable image ID, version, and digest")
	}

	if hostDistro != hostDistroAzureLinux3 ||
		qualification.Distro.Name != "Azure Linux" ||
		qualification.Distro.Version != "3.0" ||
		qualification.Distro.Version != nvidia.HostDistroVersion ||
		qualification.Distro.Kernel != kernelVersion ||
		qualification.Distro.KernelDevel != kernelVersion ||
		qualification.Distro.Kernel != nvidia.HostKernelVersion {
		return fmt.Errorf(
			"NVIDIA IMEX DRA qualification OS/kernel is %s %s %s, running host is %s %s",
			qualification.Distro.Name,
			qualification.Distro.Version,
			qualification.Distro.Kernel,
			qualificationDistroID(hostDistro),
			nvidia.HostKernelVersion,
		)
	}

	if qualification.Driver.Branch != "580" ||
		qualification.Driver.Version != driverVersion ||
		qualification.Driver.UserspaceVersion != driverVersion ||
		qualification.Driver.Package != driverPackage ||
		nvidia.DriverVersion != driverVersion {
		return fmt.Errorf(
			"NVIDIA IMEX DRA qualification driver is branch %q version %q userspace %q package %q, running NVIDIA version is %q",
			qualification.Driver.Branch,
			qualification.Driver.Version,
			qualification.Driver.UserspaceVersion,
			qualification.Driver.Package,
			nvidia.DriverVersion,
		)
	}

	if qualification.FabricManager.Package != fabricManagerPackage ||
		qualification.FabricManager.Version != driverVersion ||
		qualification.NSCQ.Package != nscqPackage ||
		qualification.NSCQ.Version != driverVersion {
		return fmt.Errorf("NVIDIA IMEX DRA qualification fabric manager or NSCQ package evidence is unsupported")
	}

	if qualification.IMEX.Package == nil ||
		qualification.IMEX.Version == nil ||
		strings.TrimSpace(*qualification.IMEX.Package) == "" ||
		*qualification.IMEX.Version != driverVersion {
		return fmt.Errorf("NVIDIA IMEX DRA qualification is blocked because approved IMEX package evidence is missing")
	}

	expectedIMEXPaths := []string{"/usr/bin/nvidia-imex", "/usr/bin/nvidia-imex-ctl"}
	if !slices.Equal(qualification.IMEX.ExpectedPaths, expectedIMEXPaths) ||
		qualification.IMEX.LifecycleOwner != "gpu-operator-dra-driverManaged" ||
		qualification.IMEX.Service.Name != "nvidia-imex.service" ||
		qualification.IMEX.Service.Enabled ||
		!qualification.IMEX.Service.Masked {
		return fmt.Errorf("NVIDIA IMEX DRA qualification IMEX paths, service ownership, or lifecycle owner is invalid")
	}

	if nvidia.NvidiaSMIPath != "/usr/bin/nvidia-smi" ||
		nvidia.NvidiaIMEXPath != expectedIMEXPaths[0] ||
		nvidia.NvidiaIMEXCtlPath != expectedIMEXPaths[1] {
		return fmt.Errorf(
			"NVIDIA IMEX DRA required binaries were not discovered at the qualified paths",
		)
	}

	if qualification.Toolkit.Version == nil ||
		qualification.Toolkit.MinimumVersion != "1.18.0" ||
		!qualification.Toolkit.Ready ||
		!versionAtLeast(*qualification.Toolkit.Version, qualification.Toolkit.MinimumVersion) {
		return fmt.Errorf("NVIDIA IMEX DRA qualification requires ready Container Toolkit 1.18.0 or later")
	}

	if qualification.ContainerRuntime.Name != "containerd" ||
		qualification.ContainerRuntime.Version == nil ||
		qualification.ContainerRuntime.MinimumVersion != "2.0.0" ||
		!versionAtLeast(*qualification.ContainerRuntime.Version, qualification.ContainerRuntime.MinimumVersion) ||
		!qualification.ContainerRuntime.CDIEnabled ||
		!validUniqueNonemptyStrings(qualification.ContainerRuntime.CDISpecDirectories, true) {
		return fmt.Errorf("NVIDIA IMEX DRA qualification requires containerd 2.0.0 or later with CDI enabled")
	}

	requiredFiles := []string{"/usr/bin/nvidia-smi", "/usr/bin/nvidia-imex", "/usr/bin/nvidia-imex-ctl"}
	requiredServices := []string{"containerd.service", "nvidia-fabricmanager.service", "nvidia-imex.service"}
	requiredDevices := []string{
		"/dev/nvidia0", "/dev/nvidia1", "/dev/nvidia2", "/dev/nvidia3",
		"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-caps",
	}

	requiredKernelModules := []string{"nvidia", "nvidia_drm", "nvidia_modeset", "nvidia_uvm", "mana", "mana_ib", "gdma"}
	if !validUniqueNonemptyStrings(qualification.Prerequisites.RequiredFiles, false) ||
		!validUniqueNonemptyStrings(qualification.Prerequisites.RequiredServices, false) ||
		!validUniqueNonemptyStrings(qualification.Prerequisites.RequiredDevices, false) ||
		!validUniqueNonemptyStrings(qualification.Prerequisites.RequiredKernelModules, false) ||
		!containsAllStrings(qualification.Prerequisites.RequiredFiles, requiredFiles) ||
		!containsAllStrings(qualification.Prerequisites.RequiredServices, requiredServices) ||
		!containsAllStrings(qualification.Prerequisites.RequiredDevices, requiredDevices) ||
		!containsAllStrings(qualification.Prerequisites.RequiredKernelModules, requiredKernelModules) {
		return fmt.Errorf("NVIDIA IMEX DRA qualification is missing required prerequisite evidence")
	}

	for _, device := range qualification.Prerequisites.RequiredDevices {
		if !slices.Contains(nvidia.GPUDevicePaths, device) {
			return fmt.Errorf("NVIDIA IMEX DRA qualification requires missing device %s", device)
		}
	}

	if qualification.Acceptance.ContractVersion != contractVersion ||
		qualification.Acceptance.ResultVersion != resultVersion ||
		qualification.Acceptance.Status != "qualified" ||
		qualification.Acceptance.QualifiedAt == nil ||
		strings.TrimSpace(*qualification.Acceptance.QualifiedAt) == "" ||
		!validUniqueNonemptyStrings(qualification.Acceptance.FailClosedReasons, false) ||
		len(qualification.Acceptance.FailClosedReasons) != 0 {
		return fmt.Errorf("NVIDIA IMEX DRA qualification acceptance status is blocked or unsupported")
	}

	evidence := qualification.Acceptance.Evidence
	if evidence.GPUCount == nil ||
		*evidence.GPUCount != 4 ||
		!evidence.FourGPUDetected ||
		nvidiaGPUCount(nvidia.GPUDevicePaths) != 4 ||
		!evidence.LocalNVLink.Checked ||
		!evidence.LocalNVLink.Healthy ||
		evidence.LocalNVLink.Evidence == nil ||
		strings.TrimSpace(*evidence.LocalNVLink.Evidence) == "" ||
		evidence.MultiNodeNVLinkClaimed ||
		!nvidia.LocalNVLinkHealthy {
		return fmt.Errorf("NVIDIA IMEX DRA qualification requires four GPUs and healthy verified local NVLink")
	}

	if qualification.Integrity.ArtifactPath != nvidiaIMEXQualificationPath ||
		qualification.Integrity.Canonicalization != "RFC8785-JCS" ||
		qualification.Integrity.Encoding != "UTF-8-without-BOM" ||
		qualification.Integrity.TrailingNewline ||
		qualification.Integrity.DigestAlgorithm != "sha256" ||
		qualification.Integrity.DigestSidecar != "nvidia-gpu-operator.json.sha256" {
		return fmt.Errorf("NVIDIA IMEX DRA qualification integrity contract is unsupported")
	}

	if qualification.Authenticity.Status != "verified" ||
		!qualification.Authenticity.RequiredBeforeRelease ||
		qualification.Authenticity.Attestation == nil ||
		strings.TrimSpace(*qualification.Authenticity.Attestation) == "" {
		return fmt.Errorf("NVIDIA IMEX DRA qualification authenticity is not verified")
	}

	return nil
}

func containsAllStrings(values, required []string) bool {
	for _, expected := range required {
		if !slices.Contains(values, expected) {
			return false
		}
	}

	return true
}

func validUniqueNonemptyStrings(values []string, requireOne bool) bool {
	if requireOne && len(values) == 0 {
		return false
	}

	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return false
		}

		if _, ok := seen[value]; ok {
			return false
		}

		seen[value] = struct{}{}
	}

	return true
}

func versionAtLeast(version, minimum string) bool {
	parse := func(value string) ([]int, bool) {
		parts := strings.Split(value, ".")

		parsed := make([]int, len(parts))
		for index, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil {
				return nil, false
			}

			parsed[index] = number
		}

		return parsed, true
	}

	current, ok := parse(version)
	if !ok {
		return false
	}

	required, ok := parse(minimum)
	if !ok {
		return false
	}

	length := max(len(current), len(required))
	for index := 0; index < length; index++ {
		var currentPart, requiredPart int
		if index < len(current) {
			currentPart = current[index]
		}

		if index < len(required) {
			requiredPart = required[index]
		}

		if currentPart != requiredPart {
			return currentPart > requiredPart
		}
	}

	return true
}

func qualificationDistroID(hostDistro string) string {
	switch hostDistro {
	case hostDistroUbuntu2404, hostDistroUbuntu2604:
		return "ubuntu"
	case hostDistroAzureLinux3:
		return "azurelinux"
	case hostDistroAzureContainerLinux:
		return "azurecontainerlinux"
	default:
		return hostDistro
	}
}

func nvidiaGPUCount(paths []string) int {
	count := 0

	for _, path := range paths {
		name := strings.TrimPrefix(filepath.Base(path), nvidiaDevPrefix)
		if _, err := strconv.Atoi(name); err == nil {
			count++
		}
	}

	return count
}

func validSHA256Digest(value string) bool {
	const prefix = "sha256:"

	digest := strings.TrimPrefix(value, prefix)
	if len(value) == len(digest) || len(digest) != 64 {
		return false
	}

	for _, char := range digest {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}

	return true
}

func nvidiaDriverMajor(version string) (int, error) {
	majorText, _, _ := strings.Cut(strings.TrimSpace(version), ".")

	major, err := strconv.Atoi(majorText)
	if err != nil {
		return 0, fmt.Errorf("NVIDIA IMEX DRA mode requires a detectable driver version, found %q", version)
	}

	return major, nil
}

// nvidiaArch contains architecture-specific values for NVIDIA library
// discovery and symlink creation inside the nspawn container.
type nvidiaArch struct {
	// ldconfigTag is the architecture identifier in ldconfig -p output
	// (e.g. "x86-64", "aarch64"). Used to filter libraries to the
	// correct architecture and avoid multilib collisions.
	ldconfigTag string

	// libDir is the Debian/Ubuntu multiarch library directory for this
	// architecture (e.g. "/usr/lib/x86_64-linux-gnu"). Symlinks to
	// bind-mounted host NVIDIA libraries are created here inside the
	// nspawn container.
	libDir string

	// i386LibDir is the optional 32-bit library directory on amd64 hosts.
	i386LibDir string
}

// nvidiaArchMap maps GOARCH values to their NVIDIA-specific arch parameters.
var nvidiaArchMap = map[string]nvidiaArch{
	"amd64": {ldconfigTag: "x86-64", libDir: "/usr/lib/x86_64-linux-gnu", i386LibDir: "/usr/lib/i386-linux-gnu"},
	"arm64": {ldconfigTag: "aarch64", libDir: "/usr/lib/aarch64-linux-gnu"},
}

// discoverNVIDIADevices scans /dev for NVIDIA device nodes and returns them
// as a sorted slice of device paths. The following device nodes are collected
// when present:
//
//   - /dev/nvidia0, /dev/nvidia1, ...  (per-GPU devices)
//   - /dev/nvidiactl                   (control device)
//   - /dev/nvidia-modeset              (modeset interface)
//   - /dev/nvidia-uvm                  (unified virtual memory)
//   - /dev/nvidia-uvm-tools            (UVM tools interface)
//   - /dev/nvidia-caps                 (capability devices)
//   - /dev/nvidia-caps-imex-channels   (IMEX channel devices)
//   - /dev/dri/card*, /dev/dri/renderD* (DRI render nodes, needed by CDI and
//     some GPU workloads such as OpenGL/Vulkan)
//
// Returns nil (not an error) when no NVIDIA devices are found; the host
// simply does not have NVIDIA GPUs or the driver is not loaded.
func discoverNVIDIADevices() []string {
	return discoverNVIDIADevicesIn(devDir)
}

func discoverNVIDIADevicesIn(deviceDir string) []string {
	var devices []string

	entries, err := os.ReadDir(deviceDir)
	if err != nil {
		return nil
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, nvidiaDevPrefix) {
			continue
		}

		// Skip directories at the top level (nvidia-caps is handled below).
		if e.IsDir() {
			continue
		}

		devices = append(devices, filepath.Join(deviceDir, name))
	}

	// Bind capability directories so devices created after nspawn starts are
	// visible inside the machine.
	capsDir := filepath.Join(deviceDir, nvidiaCapsDirName)

	if _, err := os.ReadDir(capsDir); err == nil {
		devices = append(devices, capsDir)
	}

	// Bind the IMEX channel directory for dynamically allocated channels.
	imexDir := filepath.Join(deviceDir, nvidiaIMEXDirName)

	if _, err := os.ReadDir(imexDir); err == nil {
		devices = append(devices, imexDir)
	}

	// When NVIDIA devices are present, also collect /dev/dri/* entries.
	// These DRI render nodes are created by the NVIDIA driver and are
	// referenced by the CDI specification generated by nvidia-ctk.
	// Without them, CDI-based container creation fails with ENOENT.
	if len(devices) > 0 {
		discoveredDRIDir := filepath.Join(deviceDir, filepath.Base(driDir))

		driEntries, err := os.ReadDir(discoveredDRIDir)
		if err == nil {
			for _, e := range driEntries {
				if e.IsDir() {
					continue
				}

				devices = append(devices, filepath.Join(discoveredDRIDir, e.Name()))
			}
		}
	}

	slices.Sort(devices)

	return devices
}

// nvidiaLibPrefixes are the library name prefixes collected from ldconfig output.
var nvidiaLibPrefixes = []string{
	"libnvidia-",
	"libcuda",
	"libcudadebugger",
	"libEGL_nvidia",
	"libGLX_nvidia",
	"libGLESv1_CM_nvidia",
	"libGLESv2_nvidia",
	"libnvcuvid",
	"libnvoptix",
	"libvdpau_nvidia",
}

// nvidiaLibGlobs includes both linker aliases and the real, versioned driver
// files. ldconfig -p commonly reports only the aliases, while nvidia-ctk
// explicitly looks for names such as libcuda.so.<driver-version>.
var nvidiaLibGlobs = []string{
	"libcuda.so*",
	"libcudadebugger.so*",
	"libnvidia*.so*",
	"libEGL_nvidia.so*",
	"libGLX_nvidia.so*",
	"libGLESv*_nvidia.so*",
	"libnvcuvid.so*",
	"libnvoptix.so*",
	"libvdpau_nvidia.so*",
}

// nvidiaI386LibGlobs also includes the architecture-neutral GLVND and OpenCL
// loader names needed by 32-bit applications.
var nvidiaI386LibGlobs = []string{
	"libcuda.so*",
	"libcudadebugger.so*",
	"libnvcuvid.so*",
	"libnvoptix.so*",
	"libnvidia*.so*",
	"libEGL_nvidia.so*",
	"libGLX_nvidia.so*",
	"libGLESv1_CM_nvidia.so*",
	"libGLESv2_nvidia.so*",
	"libEGL.so*",
	"libGL.so*",
	"libGLESv1_CM.so*",
	"libGLESv2.so*",
	"libGLX.so*",
	"libGLX_indirect.so*",
	"libOpenGL.so*",
	"libGLdispatch.so*",
	"libOpenCL.so*",
	"libvdpau_nvidia.so*",
}

type nvidiaLibraryResolution struct {
	libMappings      []NvidiaLibMapping
	libDirMounts     []NvidiaLibDirMount
	i386LibMappings  []NvidiaLibMapping
	i386LibDirMounts []NvidiaLibDirMount
	driverVersion    string
}

// resolveNVIDIALibraries runs ldconfig -p on the host and returns enriched
// library mappings and their corresponding bind-mount specs.
func resolveNVIDIALibraries(arch nvidiaArch) nvidiaLibraryResolution {
	out, err := exec.Command("ldconfig", "-p").Output()
	if err != nil {
		return nvidiaLibraryResolution{}
	}

	libs := parseNVIDIALibraries(out, arch.ldconfigTag)
	libs = expandNVIDIALibraries(libs)

	driverVersion := discoverNVIDIADriverVersion(libs, "/sys/module/nvidia/version")
	libs, mounts := buildNVIDIALibMounts(libs, arch.libDir)

	i386Libs, i386Mounts := resolveNVIDIAI386Libraries(driverVersion, nvidiaI386LibraryDirs(arch.i386LibDir))

	return nvidiaLibraryResolution{
		libMappings:      libs,
		libDirMounts:     mounts,
		i386LibMappings:  i386Libs,
		i386LibDirMounts: i386Mounts,
		driverVersion:    driverVersion,
	}
}

func discoverNVIDIASMI() string {
	return discoverNVIDIABinary("nvidia-smi")
}

func discoverNVIDIABinary(name string) string {
	return discoverNVIDIABinaryIn(name, []string{"/usr/bin", "/usr/local/bin", "/bin"})
}

func discoverNVIDIABinaryIn(name string, directories []string) string {
	for _, dir := range directories {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil &&
			info.Mode().IsRegular() &&
			info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}

	return ""
}

// expandNVIDIALibraries scans every library directory selected by ldconfig for
// NVIDIA aliases and versioned files. It also follows alternatives-style
// symlinks so their real targets are included in the bind mounts.
func expandNVIDIALibraries(libs []NvidiaLibMapping) []NvidiaLibMapping {
	if len(libs) == 0 {
		return nil
	}

	expanded := slices.Clone(libs)
	seenPaths := make(map[string]bool, len(libs))
	seenNames := make(map[string]bool, len(libs))
	dirs := make(map[string]bool)

	for i := range expanded {
		expanded[i].ResolvedHostPath = resolveNVIDIALibraryPath(expanded[i].HostPath)
		seenPaths[expanded[i].HostPath] = true
		seenNames[filepath.Base(expanded[i].HostPath)] = true
		dirs[filepath.Dir(expanded[i].HostPath)] = true
	}

	var matches []string

	for dir := range dirs {
		for _, pattern := range nvidiaLibGlobs {
			// VDPAU is the Video Decode and Presentation API for Unix. NVIDIA's
			// VDPAU implementation is conventionally installed in this subdirectory.
			for _, searchDir := range []string{dir, filepath.Join(dir, "vdpau")} {
				found, err := filepath.Glob(filepath.Join(searchDir, pattern))
				if err == nil {
					matches = append(matches, found...)
				}
			}
		}
	}

	sort.Strings(matches)

	add := func(path string) {
		name := filepath.Base(path)
		if seenPaths[path] || seenNames[name] || !isNVIDIALibraryName(name) {
			return
		}

		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			return
		}

		seenPaths[path] = true
		seenNames[name] = true

		expanded = append(expanded, NvidiaLibMapping{
			HostPath:         path,
			ResolvedHostPath: resolveNVIDIALibraryPath(path),
		})
	}

	for _, path := range matches {
		add(path)
	}

	// Resolve all aliases, including the original ldconfig entries. A target
	// may live outside the alias directory, for example under alternatives.
	for _, lib := range slices.Clone(expanded) {
		target, err := filepath.EvalSymlinks(lib.HostPath)
		if err == nil {
			add(target)
		}
	}

	return expanded
}

func resolveNVIDIALibraryPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}

	return path
}

func isNVIDIALibraryName(name string) bool {
	for _, pattern := range nvidiaLibGlobs {
		matched, err := filepath.Match(pattern, name)
		if err == nil && matched {
			return true
		}
	}

	return false
}

func discoverNVIDIADriverVersion(libs []NvidiaLibMapping, moduleVersionPath string) string {
	// The active SONAME symlinks are the strongest userspace signal because a
	// directory can contain files left behind by an older driver package.
	for _, preferredName := range []string{"libcuda.so.1", "libnvidia-ml.so.1"} {
		for _, lib := range libs {
			if filepath.Base(lib.HostPath) != preferredName {
				continue
			}

			target, err := filepath.EvalSymlinks(lib.HostPath)
			if err == nil {
				if version := nvidiaDriverVersionFromName(filepath.Base(target)); version != "" {
					return version
				}
			}
		}
	}

	if data, err := os.ReadFile(moduleVersionPath); err == nil {
		if version := strings.TrimSpace(string(data)); isNVIDIADriverVersion(version) {
			return version
		}
	}

	for _, preferredPrefix := range []string{"libGLX_nvidia.so.", "libcuda.so.", "libnvidia-ml.so."} {
		for _, lib := range libs {
			name := filepath.Base(lib.HostPath)
			if !strings.HasPrefix(name, preferredPrefix) {
				continue
			}

			if version := nvidiaDriverVersionFromName(name); version != "" {
				return version
			}
		}
	}

	return ""
}

func nvidiaDriverVersionFromName(name string) string {
	_, version, found := strings.Cut(name, ".so.")
	if found && isNVIDIADriverVersion(version) {
		return version
	}

	return ""
}

func isNVIDIADriverVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}

	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return false
		}
	}

	return true
}

func nvidiaI386LibraryDirs(i386LibDir string) []string {
	if i386LibDir == "" {
		return nil
	}

	dirs := []string{i386LibDir}
	if strings.HasPrefix(i386LibDir, "/usr/") {
		dirs = append(dirs, strings.TrimPrefix(i386LibDir, "/usr"))
	}

	return dirs
}

// resolveNVIDIAI386Libraries finds a compat32 installation matching the active
// driver version. This intentionally scans the filesystem because ldconfig -p
// often lists only its SONAME aliases.
func resolveNVIDIAI386Libraries(driverVersion string, candidateDirs []string) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	if driverVersion == "" {
		return nil, nil
	}

	seenDirs := make(map[string]bool)
	seenNames := make(map[string]bool)

	var libs []NvidiaLibMapping

	for _, candidateDir := range candidateDirs {
		dir, err := filepath.EvalSymlinks(candidateDir)
		if err != nil || seenDirs[dir] || !hasNVIDIADriverVersionInDir(dir, driverVersion) {
			continue
		}

		seenDirs[dir] = true

		var matches []string

		for _, pattern := range nvidiaI386LibGlobs {
			// VDPAU is the Video Decode and Presentation API for Unix. NVIDIA's
			// VDPAU implementation is conventionally installed in this subdirectory.
			for _, searchDir := range []string{dir, filepath.Join(dir, "vdpau")} {
				found, globErr := filepath.Glob(filepath.Join(searchDir, pattern))
				if globErr == nil {
					matches = append(matches, found...)
				}
			}
		}

		sort.Strings(matches)

		for _, path := range matches {
			name := filepath.Base(path)
			if seenNames[name] {
				continue
			}

			info, statErr := os.Lstat(path)
			if statErr != nil || info.IsDir() {
				continue
			}

			seenNames[name] = true

			libs = append(libs, NvidiaLibMapping{
				HostPath:         path,
				ResolvedHostPath: resolveNVIDIALibraryPath(path),
			})
		}
	}

	return buildNVIDIALibMountsAt(libs, "/usr/lib/i386-linux-gnu", NvidiaHostI386LibDir)
}

func hasNVIDIADriverVersionInDir(dir, driverVersion string) bool {
	for _, family := range []string{"libGLX_nvidia.so.", "libnvidia-ml.so.", "libcuda.so."} {
		path := filepath.Join(dir, family+driverVersion)
		if info, err := os.Lstat(path); err == nil && !info.IsDir() {
			return true
		}
	}

	return false
}

// buildNVIDIALibMounts takes parsed library mappings (from parseNVIDIALibraries),
// deduplicates their parent directories into bind-mount specs, and stamps each
// mapping with its container-side path. containerLibDir is the multiarch
// library directory inside the container where symlinks will be created.
func buildNVIDIALibMounts(libs []NvidiaLibMapping, containerLibDir string) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	return buildNVIDIALibMountsAt(libs, containerLibDir, NvidiaHostLibDir)
}

func buildNVIDIALibMountsAt(libs []NvidiaLibMapping, containerLibDir, mountBaseDir string) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	if len(libs) == 0 {
		return nil, nil
	}

	// Collect unique host directories and sort for deterministic index
	// assignment regardless of ldconfig output ordering.
	seen := make(map[string]bool)

	var dirs []string

	for _, lib := range libs {
		dir := filepath.Dir(nvidiaLibSourceHostPath(lib))
		if seen[dir] {
			continue
		}

		seen[dir] = true
		dirs = append(dirs, dir)
	}

	sort.Strings(dirs)

	// Build mounts and a dir → container-dir lookup in one pass.
	dirToContainer := make(map[string]string, len(dirs))
	mounts := make([]NvidiaLibDirMount, len(dirs))

	for i, dir := range dirs {
		containerDir := fmt.Sprintf("%s/%d", mountBaseDir, i)
		mounts[i] = NvidiaLibDirMount{
			Index:        i,
			HostDir:      dir,
			ContainerDir: containerDir,
		}

		dirToContainer[dir] = containerDir
	}

	// Stamp each library mapping with its resolved container source and its
	// original destination name. This avoids depending on absolute host
	// symlinks resolving inside the nspawn machine.
	for i := range libs {
		sourcePath := nvidiaLibSourceHostPath(libs[i])
		libs[i].ContainerPath = filepath.Join(
			dirToContainer[filepath.Dir(sourcePath)],
			filepath.Base(sourcePath),
		)
		libs[i].LinkPath = filepath.Join(containerLibDir, filepath.Base(libs[i].HostPath))
	}

	return libs, mounts
}

func nvidiaLibSourceHostPath(lib NvidiaLibMapping) string {
	if lib.ResolvedHostPath != "" {
		return lib.ResolvedHostPath
	}

	return lib.HostPath
}

// parseNVIDIALibraries extracts NVIDIA library mappings from ldconfig -p
// output. Only libraries matching the given architecture tag (e.g. "x86-64",
// "aarch64") are included to avoid multilib collisions.
func parseNVIDIALibraries(ldconfigOutput []byte, archTag string) []NvidiaLibMapping {
	var libs []NvidiaLibMapping

	seen := make(map[string]bool)

	scanner := bufio.NewScanner(bytes.NewReader(ldconfigOutput))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// ldconfig -p lines look like:
		//   libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
		if !strings.Contains(line, "=>") {
			continue
		}

		// Filter to the target architecture only when an architecture was given.
		if archTag != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(archTag)) {
			continue
		}

		isNvidia := false

		for _, prefix := range nvidiaLibPrefixes {
			if strings.HasPrefix(line, prefix) {
				isNvidia = true
				break
			}
		}

		if !isNvidia {
			continue
		}

		// Extract the path after "=> ".
		parts := strings.SplitN(line, "=> ", 2)
		if len(parts) != 2 {
			continue
		}

		hostPath := strings.TrimSpace(parts[1])
		if hostPath == "" {
			continue
		}

		basename := filepath.Base(hostPath)

		// Deduplicate by basename for an architecture-specific lookup, where
		// ldconfig priority determines the preferred library. Keep paths
		// distinct when collecting optional i386 directories.
		key := basename
		if archTag == "" {
			key = hostPath
		}

		if seen[key] {
			continue
		}

		seen[key] = true

		libs = append(libs, NvidiaLibMapping{HostPath: hostPath})
	}

	return libs
}
