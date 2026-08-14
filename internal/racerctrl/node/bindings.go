// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// Durable device bindings.
//
// A ublk minor is a node-local decision the agent makes at NodeStageVolume
// time, and it is the only piece of the node's state that cannot be derived
// from the cluster: the config records that device 4 carries extents 9 and 10,
// but nothing in it records that device 4 is the volume pv-abc. Keeping that
// map only in memory means a racer-ctrl restart forgets which volume every
// running pod is using, and the next render publishes a config with no devices
// at all - which takes the exports away from pods that are still writing to
// them, on a node where racer itself never restarted.
//
// The kubelet does not save us here. It re-issues NodeStageVolume after the
// *node* restarts, but a container restart of the plugin alone is not an event
// it replays staging for, and even when it does replay it, it does so long
// after the agent has already published the empty config.
//
// So the map is written next to the config, in the same emptyDir that carries
// the config to racer. That is exactly the right lifetime: the directory lives
// as long as the pod, which is as long as racer's exports do. If the pod is
// replaced the file is gone, and so are the ublk devices it described.
//
// racer's inotify watch matches on the config file's name (see Watch::wait in
// cmd/racer/src/config.rs), so a sibling file in the same directory is not a
// reload trigger.

// BindingsFileName is the file the agent records its minors in, alongside the
// config in the watched directory.
const BindingsFileName = "bindings.json"

// storedBindings is the on-disk schema. It is deliberately its own type rather
// than the in-memory NodeState fields: what is written here is a file format
// that has to survive an upgrade, and the scraped and derived halves of
// NodeState have no business in it.
type storedBindings struct {
	// Devices are the volumes this node exports and the minors it exports them
	// on.
	Devices []storedDevice `json:"devices"`

	// Fabric are the minors this node publishes universe namespaces from. They
	// are recorded for the same reason as the devices: peers hold NVMe
	// namespaces pointing at /dev/ublkb<minor>, and re-deriving the minor from
	// a lowest-free scan after a restart could hand a universe a different one
	// and silently repoint every peer.
	Fabric []storedFabric `json:"fabric"`
}

type storedDevice struct {
	DeviceID uint32 `json:"deviceId"`
	Volume   string `json:"volume"`
}

type storedFabric struct {
	UniverseID uint32 `json:"universeId"`
	DeviceID   uint32 `json:"deviceId"`
}

// BindingsPath is where the agent records its device bindings.
func (c Config) BindingsPath() string {
	return c.ConfigDir + "/" + BindingsFileName
}

// readBindings loads the bindings an earlier run left behind.
//
// A missing file is not an error: it is a pod that has never staged anything.
// A corrupt one is, because the caller has to decide whether to carry on with
// nothing - which is the same as forgetting - or refuse to start.
func readBindings(path string) (storedBindings, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return storedBindings{}, nil
	}

	if err != nil {
		return storedBindings{}, fmt.Errorf("read %s: %w", path, err)
	}

	var stored storedBindings
	if err := json.Unmarshal(data, &stored); err != nil {
		return storedBindings{}, fmt.Errorf("parse %s: %w", path, err)
	}

	for _, device := range stored.Devices {
		if device.DeviceID == 0 || device.Volume == "" {
			return storedBindings{}, fmt.Errorf("%s names a device with no minor or no volume", path)
		}
	}

	for _, export := range stored.Fabric {
		if export.DeviceID == 0 || export.UniverseID == 0 {
			return storedBindings{}, fmt.Errorf("%s names a fabric export with no minor or no universe", path)
		}
	}

	return stored, nil
}

// writeBindings records the bindings, atomically. A half-written file read back
// after a crash would be worse than no file at all: it would describe some of
// the exports and quietly drop the rest.
func writeBindings(path string, self racerctrl.NodeState) error {
	stored := storedBindings{
		Devices: make([]storedDevice, 0, len(self.Devices)),
		Fabric:  make([]storedFabric, 0, len(self.Fabric)),
	}

	for _, binding := range self.Devices {
		stored.Devices = append(stored.Devices, storedDevice{
			DeviceID: binding.DeviceID,
			Volume:   binding.Volume,
		})
	}

	for _, export := range self.Fabric {
		stored.Fabric = append(stored.Fabric, storedFabric{
			UniverseID: export.UniverseID,
			DeviceID:   export.DeviceID,
		})
	}

	sort.Slice(stored.Devices, func(i, j int) bool {
		return stored.Devices[i].DeviceID < stored.Devices[j].DeviceID
	})
	sort.Slice(stored.Fabric, func(i, j int) bool {
		return stored.Fabric[i].UniverseID < stored.Fabric[j].UniverseID
	})

	data, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode bindings: %w", err)
	}

	return racerctrl.WriteFileAtomic(path, data)
}

// apply installs the stored bindings on a node state.
func (s storedBindings) apply(self *racerctrl.NodeState) {
	self.Devices = make([]racerctrl.DeviceBinding, 0, len(s.Devices))
	for _, device := range s.Devices {
		self.Devices = append(self.Devices, racerctrl.DeviceBinding{
			DeviceID: device.DeviceID,
			Volume:   device.Volume,
		})
	}

	self.Fabric = make([]racerctrl.FabricExport, 0, len(s.Fabric))
	for _, export := range s.Fabric {
		self.Fabric = append(self.Fabric, racerctrl.FabricExport{
			UniverseID: export.UniverseID,
			DeviceID:   export.DeviceID,
		})
	}
}
