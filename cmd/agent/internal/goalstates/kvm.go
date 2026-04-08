package goalstates

import (
	"os"
	"slices"
)

// KVM host discovery and goal state resolution.
//
// When KVM hardware virtualization is available on the host, /dev/kvm must be
// bind-mounted into the nspawn container and allowed via the cgroup device
// controller so that KubeVirt's virt-launcher pods can start QEMU with
// hardware acceleration. Similarly, /dev/net/tun is needed for creating TAP
// interfaces for VM networking.
//
// Unlike NVIDIA devices, the KVM device set is small and well-known, so
// discovery simply probes for each path rather than scanning directories.

// KVMHost aggregates virtualization device paths discovered on the host. When
// non-empty, the nspawn configuration will bind-mount these devices and grant
// the container cgroup access to them.
type KVMHost struct {
	// DevicePaths lists virtualization device paths discovered on the host
	// (e.g. /dev/kvm, /dev/net/tun). When non-empty the nspawn
	// configuration will bind-mount these devices and grant the container
	// cgroup access to them via DeviceAllow.
	DevicePaths []string
}

// kvmDevicePaths lists the device nodes probed for KVM/virtualization support.
// Each path is checked independently; only those that exist as device nodes
// are included in the result.
//
//   - /dev/kvm       — KVM hardware acceleration (virt-launcher uses this for
//     QEMU with hardware-assisted virtualization)
//   - /dev/net/tun   — TUN/TAP device for creating virtual network interfaces
//     (virt-launcher creates TAP interfaces for VM networking)
var kvmDevicePaths = []string{
	"/dev/kvm",
	"/dev/net/tun",
}

// ResolveKVMHost probes the host for KVM/virtualization device nodes and
// returns a KVMHost with the paths that exist. Returns a zero-value KVMHost
// (empty DevicePaths) on hosts without KVM support — this is not an error.
func ResolveKVMHost() KVMHost {
	var devices []string

	for _, path := range kvmDevicePaths {
		if isDeviceNode(path) {
			devices = append(devices, path)
		}
	}

	slices.Sort(devices)

	return KVMHost{DevicePaths: devices}
}

// isDeviceNode returns true if the given path exists and is a device node
// (character or block device).
func isDeviceNode(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}

	return fi.Mode().Type()&(os.ModeDevice|os.ModeCharDevice) != 0
}
