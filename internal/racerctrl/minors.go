// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"
)

// Node-local device ids.
//
// A device id is a ublk minor: the device racer exports for it is /dev/ublkb<id>.
// Both kinds of export draw from one node-local space - one id per configured
// device, one per universe for that universe's fabric namespace - which is why
// R3 states the budget as len(universes) + len(devices).
//
// Making the id the minor rather than letting the kernel choose is what makes
// R7's stability requirement hold by construction: a volume's path is a function
// of its binding, so a reload that does not change the binding cannot change the
// path, and an open fd survives.
//
// Ids are assigned lowest-free rather than by a bump cursor. Minors are a small
// space that is genuinely reclaimed when a volume leaves the node, and unlike
// extent ids a reused minor is harmless: nothing persists that refers to it.

// MinDeviceID is the lowest legal ublk minor. Zero is reserved everywhere in the
// schema, so the space is 1..MaxExports.
const MinDeviceID = 1

// The minor space is the kernel's, not the node's.
//
// A ublk minor is global to the machine that runs the driver, and racer asks the
// kernel for a named one rather than letting it choose, so two racer instances
// sharing a kernel that both start counting at MinDeviceID collide on their very
// first device. One racer per kernel is the deployment this project targets and
// there the default floor is right, but it is not the only arrangement that
// happens: test harnesses run a whole zone of instances against one kernel, and a
// box may already run another ublk user that owns the low minors.
//
// So the floor is a parameter. Each instance gets MaxExports ids from its base,
// which keeps the per-node budget R3 states intact while letting several
// instances hold disjoint slices. A zero base means MinDeviceID.

// MinorSpace is where a node draws its ublk minors from.
type MinorSpace struct {
	// Base is the floor this instance allocates from; the window it may use is
	// Base..Base+MaxExports-1. Zero means MinDeviceID.
	Base uint32

	// InUse, when set, reports whether the kernel already holds a minor. It is
	// consulted only for minors this node has not bound itself, so it answers
	// exactly one question: has somebody else taken this one? Somebody else is
	// another racer instance on the same kernel, an unrelated ublk user, or a
	// device leaked by an instance that died before it could delete it. All
	// three make racer's CMD_ADD_DEV fail on that minor, and all three are
	// invisible to the bindings this node keeps.
	//
	// A nil probe means the node assumes it owns the whole window, which is the
	// right assumption for one racer per kernel and the wrong one everywhere
	// else.
	InUse func(id uint32) bool
}

// window is the minor range this space covers.
func (s MinorSpace) window() (uint32, uint32) {
	base := s.Base
	if base < MinDeviceID {
		base = MinDeviceID
	}

	return base, base + MaxExports - 1
}

// AssignFabricDeviceID returns the minor this node publishes a universe's fabric
// namespace on, allocating one if the universe is new to this node. It reports
// whether anything changed.
func AssignFabricDeviceID(self *NodeState, universeID uint32, space MinorSpace) (uint32, bool, error) {
	if universeID == 0 {
		return 0, false, fmt.Errorf("universe id must not be zero")
	}

	for _, export := range self.Fabric {
		if export.UniverseID == universeID {
			return export.DeviceID, false, nil
		}
	}

	id, err := lowestFreeMinor(self, space)
	if err != nil {
		return 0, false, fmt.Errorf("universe %d fabric: %w", universeID, err)
	}

	self.Fabric = append(self.Fabric, FabricExport{UniverseID: universeID, DeviceID: id})
	sortFabric(self.Fabric)

	return id, true, nil
}

// AssignDeviceID returns the minor this node exports a volume on, allocating one
// if the volume is new to this node. It reports whether anything changed.
func AssignDeviceID(self *NodeState, volume string, space MinorSpace) (uint32, bool, error) {
	if volume == "" {
		return 0, false, fmt.Errorf("volume name must not be empty")
	}

	for _, binding := range self.Devices {
		if binding.Volume == volume {
			return binding.DeviceID, false, nil
		}
	}

	id, err := lowestFreeMinor(self, space)
	if err != nil {
		return 0, false, fmt.Errorf("volume %q: %w", volume, err)
	}

	self.Devices = append(self.Devices, DeviceBinding{DeviceID: id, Volume: volume})
	sortDevices(self.Devices)

	return id, true, nil
}

// ReleaseDeviceID drops a volume's binding, returning the minor to the free
// space. It reports whether anything changed.
func ReleaseDeviceID(self *NodeState, volume string) bool {
	for i, binding := range self.Devices {
		if binding.Volume == volume {
			self.Devices = append(self.Devices[:i:i], self.Devices[i+1:]...)

			return true
		}
	}

	return false
}

// ReleaseFabricDeviceID drops a universe's fabric export. It reports whether
// anything changed.
func ReleaseFabricDeviceID(self *NodeState, universeID uint32) bool {
	for i, export := range self.Fabric {
		if export.UniverseID == universeID {
			self.Fabric = append(self.Fabric[:i:i], self.Fabric[i+1:]...)

			return true
		}
	}

	return false
}

func lowestFreeMinor(self *NodeState, space MinorSpace) (uint32, error) {
	taken := make(map[uint32]struct{}, len(self.Devices)+len(self.Fabric))

	for _, binding := range self.Devices {
		taken[binding.DeviceID] = struct{}{}
	}

	for _, export := range self.Fabric {
		taken[export.DeviceID] = struct{}{}
	}

	first, last := space.window()

	for id := first; id <= last; id++ {
		if _, ok := taken[id]; ok {
			continue
		}

		if space.InUse != nil && space.InUse(id) {
			continue
		}

		return id, nil
	}

	return 0, fmt.Errorf("node has no free device id in %d..%d; all %d exports are in use or held elsewhere",
		first, last, MaxExports)
}

func sortDevices(devices []DeviceBinding) {
	sort.Slice(devices, func(i, j int) bool { return devices[i].DeviceID < devices[j].DeviceID })
}

func sortFabric(exports []FabricExport) {
	sort.Slice(exports, func(i, j int) bool { return exports[i].UniverseID < exports[j].UniverseID })
}

// BlockDevicePath is where racer exports a device id.
func BlockDevicePath(id uint32) string {
	return fmt.Sprintf("/dev/ublkb%d", id)
}
