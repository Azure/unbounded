// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"unicode"
)

const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"

	defaultName        = "playpen"
	defaultCPUs        = 2
	defaultMemory      = "2048M"
	defaultVXLANVNI    = 1
	defaultVXLANPort   = 4789
	defaultMTU         = 1360
	defaultBridgeName  = "br-playpen"
	defaultTapName     = "tap0"
	defaultVXLANName   = "vxlan0"
	defaultRuntimeDir  = "/run/playpen"
	defaultDiskPath    = "/var/lib/playpen/disk.raw"
	defaultDiskSize    = "20G"
	defaultSWTPMBinary = "swtpm"
	defaultTPMStateDir = "/var/lib/playpen/tpm"
	defaultTPMSocket   = "/run/playpen/swtpm.sock"
	defaultBMCListen   = ":8443"
	defaultBMCUsername = "admin"
	defaultBMCPassword = "playpen"
	defaultBMCDeviceID = "1"
	defaultBMCCertPath = "/var/lib/playpen/redfish.crt"
	defaultBMCKeyPath  = "/var/lib/playpen/redfish.key"
	defaultKVMPath     = "/dev/kvm"
	defaultTUNPath     = "/dev/net/tun"

	maxInterfaceNameLen = 15
	maxVXLANVNI         = 1<<24 - 1
)

// Config is the complete playpen runtime configuration.
type Config struct {
	Name          string
	Arch          string
	CPUs          int
	Memory        string
	QEMUBinary    string
	VXLANRemote   string
	VXLANLocal    string
	VXLANVNI      int
	VXLANPort     int
	MTU           int
	BridgeName    string
	TapName       string
	VXLANName     string
	MAC           string
	MACIdentity   string
	UEFICode      string
	UEFIVars      string
	RuntimeDir    string
	DiskPath      string
	DiskSize      string
	SWTPMBinary   string
	TPMStateDir   string
	TPMSocket     string
	BMCListen     string
	BMCUsername   string
	BMCPassword   string
	BMCDeviceID   string
	BMCCertPath   string
	BMCKeyPath    string
	KVMPath       string
	TUNPath       string
	ExtraQEMUArgs []string
}

// DefaultConfig returns the default playpen configuration without generating
// runtime-only values such as the VM MAC address.
func DefaultConfig() Config {
	return Config{
		Name:        defaultName,
		Arch:        runtime.GOARCH,
		CPUs:        defaultCPUs,
		Memory:      defaultMemory,
		VXLANVNI:    defaultVXLANVNI,
		VXLANPort:   defaultVXLANPort,
		MTU:         defaultMTU,
		BridgeName:  defaultBridgeName,
		TapName:     defaultTapName,
		VXLANName:   defaultVXLANName,
		RuntimeDir:  defaultRuntimeDir,
		DiskPath:    defaultDiskPath,
		DiskSize:    defaultDiskSize,
		SWTPMBinary: defaultSWTPMBinary,
		TPMStateDir: defaultTPMStateDir,
		TPMSocket:   defaultTPMSocket,
		BMCListen:   defaultBMCListen,
		BMCUsername: defaultBMCUsername,
		BMCPassword: defaultBMCPassword,
		BMCDeviceID: defaultBMCDeviceID,
		BMCCertPath: defaultBMCCertPath,
		BMCKeyPath:  defaultBMCKeyPath,
		KVMPath:     defaultKVMPath,
		TUNPath:     defaultTUNPath,
	}
}

// Normalize returns a copy of cfg with defaults applied and derived values
// resolved. It does not check host devices or firmware files.
func Normalize(cfg Config) (Config, error) {
	defaults := DefaultConfig()

	if cfg.Name == "" {
		cfg.Name = defaults.Name
	}

	if cfg.Arch == "" {
		cfg.Arch = defaults.Arch
	}

	arch, err := normalizeArch(cfg.Arch)
	if err != nil {
		return Config{}, err
	}

	cfg.Arch = arch

	if cfg.CPUs == 0 {
		cfg.CPUs = defaults.CPUs
	}

	if cfg.Memory == "" {
		cfg.Memory = defaults.Memory
	}

	if cfg.QEMUBinary == "" {
		cfg.QEMUBinary = defaultQEMUBinary(cfg.Arch)
	}

	if cfg.VXLANVNI == 0 {
		cfg.VXLANVNI = defaults.VXLANVNI
	}

	if cfg.VXLANPort == 0 {
		cfg.VXLANPort = defaults.VXLANPort
	}

	if cfg.MTU == 0 {
		cfg.MTU = defaults.MTU
	}

	if cfg.BridgeName == "" {
		cfg.BridgeName = defaults.BridgeName
	}

	if cfg.TapName == "" {
		cfg.TapName = defaults.TapName
	}

	if cfg.VXLANName == "" {
		cfg.VXLANName = defaults.VXLANName
	}

	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = defaults.RuntimeDir
	}

	if cfg.DiskPath == "" {
		cfg.DiskPath = defaults.DiskPath
	}

	if cfg.DiskSize == "" {
		cfg.DiskSize = defaults.DiskSize
	}

	if cfg.SWTPMBinary == "" {
		cfg.SWTPMBinary = defaults.SWTPMBinary
	}

	if cfg.TPMStateDir == "" {
		cfg.TPMStateDir = defaults.TPMStateDir
	}

	if cfg.TPMSocket == "" {
		cfg.TPMSocket = defaults.TPMSocket
	}

	if cfg.BMCListen == "" {
		cfg.BMCListen = defaults.BMCListen
	}

	if cfg.BMCUsername == "" {
		cfg.BMCUsername = defaults.BMCUsername
	}

	if cfg.BMCPassword == "" {
		cfg.BMCPassword = defaults.BMCPassword
	}

	if cfg.BMCDeviceID == "" {
		cfg.BMCDeviceID = defaults.BMCDeviceID
	}

	if cfg.BMCCertPath == "" {
		cfg.BMCCertPath = defaults.BMCCertPath
	}

	if cfg.BMCKeyPath == "" {
		cfg.BMCKeyPath = defaults.BMCKeyPath
	}

	if cfg.KVMPath == "" {
		cfg.KVMPath = defaults.KVMPath
	}

	if cfg.TUNPath == "" {
		cfg.TUNPath = defaults.TUNPath
	}

	if cfg.MAC == "" {
		cfg.MACIdentity = strings.TrimSpace(cfg.MACIdentity)
		if cfg.MACIdentity != "" {
			cfg.MAC = MACFromIdentity(cfg.MACIdentity).String()
		} else {
			mac, err := GenerateMAC()
			if err != nil {
				return Config{}, err
			}

			cfg.MAC = mac.String()
		}
	}

	return cfg, validate(cfg)
}

func validate(cfg Config) error {
	var errs []error

	if strings.TrimSpace(cfg.Name) == "" {
		errs = append(errs, errors.New("name must not be empty"))
	}

	if cfg.CPUs < 1 {
		errs = append(errs, fmt.Errorf("cpus must be greater than zero: %d", cfg.CPUs))
	}

	if !validMemory(cfg.Memory) {
		errs = append(errs, fmt.Errorf("memory must be a positive QEMU memory size: %q", cfg.Memory))
	}

	if strings.TrimSpace(cfg.DiskPath) == "" {
		errs = append(errs, errors.New("disk path must not be empty"))
	}

	if _, err := parseDiskSize(cfg.DiskSize); err != nil {
		errs = append(errs, err)
	}

	if strings.TrimSpace(cfg.SWTPMBinary) == "" {
		errs = append(errs, errors.New("swtpm binary must not be empty"))
	}

	if strings.TrimSpace(cfg.TPMStateDir) == "" {
		errs = append(errs, errors.New("TPM state directory must not be empty"))
	}

	if strings.TrimSpace(cfg.TPMSocket) == "" {
		errs = append(errs, errors.New("TPM socket path must not be empty"))
	}

	if _, err := net.ResolveTCPAddr("tcp", cfg.BMCListen); err != nil {
		errs = append(errs, fmt.Errorf("BMC listen address is invalid: %w", err))
	}

	for label, value := range map[string]string{
		"BMC username":         cfg.BMCUsername,
		"BMC password":         cfg.BMCPassword,
		"BMC device ID":        cfg.BMCDeviceID,
		"BMC certificate path": cfg.BMCCertPath,
		"BMC key path":         cfg.BMCKeyPath,
	} {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Errorf("%s must not be empty", label))
		}
	}

	if net.ParseIP(cfg.VXLANRemote) == nil {
		if cfg.VXLANRemote == "" {
			errs = append(errs, errors.New("vxlan-remote is required"))
		} else {
			errs = append(errs, fmt.Errorf("vxlan-remote must be an IP address: %q", cfg.VXLANRemote))
		}
	}

	if cfg.VXLANLocal != "" && net.ParseIP(cfg.VXLANLocal) == nil {
		errs = append(errs, fmt.Errorf("vxlan-local must be an IP address: %q", cfg.VXLANLocal))
	}

	if cfg.VXLANVNI < 1 || cfg.VXLANVNI > maxVXLANVNI {
		errs = append(errs, fmt.Errorf("vxlan-vni must be between 1 and %d: %d", maxVXLANVNI, cfg.VXLANVNI))
	}

	if cfg.VXLANPort < 1 || cfg.VXLANPort > 65535 {
		errs = append(errs, fmt.Errorf("vxlan-port must be between 1 and 65535: %d", cfg.VXLANPort))
	}

	if cfg.MTU < 576 || cfg.MTU > 65535 {
		errs = append(errs, fmt.Errorf("mtu must be between 576 and 65535: %d", cfg.MTU))
	}

	for label, name := range map[string]string{
		"bridge": cfg.BridgeName,
		"tap":    cfg.TapName,
		"vxlan":  cfg.VXLANName,
	} {
		if err := validateInterfaceName(label, name); err != nil {
			errs = append(errs, err)
		}
	}

	if cfg.BridgeName == cfg.TapName || cfg.BridgeName == cfg.VXLANName || cfg.TapName == cfg.VXLANName {
		errs = append(errs, errors.New("bridge, tap, and vxlan interface names must be distinct"))
	}

	if _, err := ParseMAC(cfg.MAC); err != nil {
		errs = append(errs, fmt.Errorf("mac: %w", err))
	}

	return errors.Join(errs...)
}

func normalizeArch(arch string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case ArchAMD64, "x86_64":
		return ArchAMD64, nil
	case ArchARM64, "aarch64":
		return ArchARM64, nil
	default:
		return "", fmt.Errorf("unsupported arch %q: expected amd64 or arm64", arch)
	}
}

func defaultQEMUBinary(arch string) string {
	switch arch {
	case ArchAMD64:
		return "qemu-system-x86_64"
	case ArchARM64:
		return "qemu-system-aarch64"
	default:
		return "qemu-system-" + arch
	}
}

func validMemory(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}

	numberEnd := 0
	for numberEnd < len(value) && value[numberEnd] >= '0' && value[numberEnd] <= '9' {
		numberEnd++
	}

	if numberEnd == 0 {
		return false
	}

	n, err := strconv.Atoi(value[:numberEnd])
	if err != nil || n < 1 {
		return false
	}

	suffix := strings.ToUpper(value[numberEnd:])
	if suffix == "" {
		return true
	}

	if len(suffix) == 1 && strings.Contains("KMGTP", suffix) {
		return true
	}

	if len(suffix) == 2 && suffix[1] == 'B' && strings.Contains("KMGTP", suffix[:1]) {
		return true
	}

	return false
}

func parseDiskSize(value string) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return 0, fmt.Errorf("disk size must be a positive size: %q", value)
	}

	numberEnd := 0
	for numberEnd < len(value) && value[numberEnd] >= '0' && value[numberEnd] <= '9' {
		numberEnd++
	}

	if numberEnd == 0 {
		return 0, fmt.Errorf("disk size must be a positive size: %q", value)
	}

	n, err := strconv.ParseInt(value[:numberEnd], 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("disk size must be a positive size: %q", value)
	}

	suffix := strings.ToUpper(value[numberEnd:])
	if len(suffix) == 2 && suffix[1] == 'B' {
		suffix = suffix[:1]
	}

	multiplier := int64(1)

	switch suffix {
	case "":
	case "K":
		multiplier = 1 << 10
	case "M":
		multiplier = 1 << 20
	case "G":
		multiplier = 1 << 30
	case "T":
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("disk size must be a positive size: %q", value)
	}

	if n > int64(^uint64(0)>>1)/multiplier {
		return 0, fmt.Errorf("disk size is too large: %q", value)
	}

	return n * multiplier, nil
}

func validateInterfaceName(label, name string) error {
	if name == "" {
		return fmt.Errorf("%s interface name must not be empty", label)
	}

	if len(name) > maxInterfaceNameLen {
		return fmt.Errorf("%s interface name %q exceeds %d bytes", label, name, maxInterfaceNameLen)
	}

	if name == "." || name == ".." || strings.ContainsAny(name, "/:") {
		return fmt.Errorf("%s interface name %q is invalid", label, name)
	}

	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("%s interface name %q contains invalid whitespace or control characters", label, name)
		}
	}

	return nil
}
