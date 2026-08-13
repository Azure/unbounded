// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
	"github.com/Azure/unbounded/internal/racerctrl"
)

// imagePublishInterval is how often the image device map is reconsidered.
//
// It is a poll rather than an edge because the thing it waits for produces no
// event: the device node appears when racer has acted on a config this agent
// already published, and nothing in the cluster changes when it does. A
// reconcile is only triggered by an apiserver watch or by a change in scraped
// health, so an agent that published the map only from Reconcile would leave it
// missing entries until something unrelated happened to move.
const imagePublishInterval = 5 * time.Second

// blockDevicePath is where a device id's block device appears.
//
// It resolves through devRoot rather than calling racerctrl.BlockDevicePath so
// that a test can point it at a directory it controls. The publisher only names
// a device once its node exists, and that check is the interesting half of the
// behaviour.
func blockDevicePath(id uint32) string {
	return filepath.Join(devRoot, fmt.Sprintf("ublkb%d", id))
}

// imageMember is one volume of the cluster image address space as this node
// sees it: which extent it is cluster-wide, and which of this node's ublk
// minors carries it.
type imageMember struct {
	// Volume is the PersistentVolume name, used only for logs.
	Volume string

	// Role is racerctrl.ImageRoleSegment or racerctrl.ImageRoleCatalog.
	Role string

	// ExtentID is the volume's single extent. It is the cluster-wide name of a
	// segment, and it is what a catalog record addresses. The device minor
	// cannot serve that purpose because minors are node-local.
	ExtentID uint32

	// DeviceID is this node's ublk minor for the volume.
	DeviceID uint32

	// Bytes is the volume's size.
	Bytes uint64
}

// imageState is everything the publisher needs, captured under the agent's
// mutex so the loop that writes the file never touches agent state.
type imageState struct {
	universe uint32
	members  []imageMember
}

// imageVolumeRoles returns the role of every PersistentVolume that declares
// itself part of the image volume.
//
// The role lives on the PV rather than on the StorageClass because a universe
// holds ordinary volumes too, and rather than in a CRD because everything else
// about a racer volume is already an annotation on the object it describes.
func imageVolumeRoles(volumes []*corev1.PersistentVolume) map[string]string {
	roles := map[string]string{}

	for _, pv := range volumes {
		if pv == nil || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != racerctrl.DriverName {
			continue
		}

		switch role := pv.Annotations[racerctrl.ImageRoleAnnotation]; role {
		case racerctrl.ImageRoleSegment, racerctrl.ImageRoleCatalog:
			roles[pv.Name] = role
		}
	}

	return roles
}

// reconcileImageVolumes binds a device to every image volume the cluster has,
// and releases the ones it no longer has.
//
// This is the whole of why an image volume needs no CSI staging. A node joins a
// universe if it exports one of that universe's volumes, so binding a device
// here is enough to make the derivation ship the extent and hand racer a device
// to serve it on. Every node does this for every image volume, unconditionally,
// which is exactly the many-reader shape the snapshotter needs: a layer written
// once on one node is readable on all of them without anything having been
// scheduled.
//
// Only volumes the cluster view already carries are considered, which means
// only volumes the operator has finished allocating. Binding one earlier would
// make every render fail, because a device whose volume has no composition
// names no extents.
func (a *Agent) reconcileImageVolumes(cluster racerctrl.ClusterState, roles map[string]string) {
	if len(roles) == 0 && len(a.imageBound) == 0 {
		return
	}

	space := a.minorSpace()
	wanted := map[string]struct{}{}

	state := imageState{}

	for _, universe := range cluster.Universes {
		for _, volume := range universe.Volumes {
			role, ok := roles[volume.Name]
			if !ok {
				continue
			}

			if len(volume.Composition) != 1 {
				a.log.Warn("ignoring image volume, it is not a single extent",
					"volume", volume.Name, "extents", len(volume.Composition))

				continue
			}

			// One universe. The snapshotter addresses a segment by id within a
			// single address space and has nowhere to put a second one, so a
			// second universe's image volumes are not silently mixed in.
			if state.universe != 0 && state.universe != universe.ID {
				a.log.Warn("ignoring image volume from a second universe",
					"volume", volume.Name, "universe", universe.ID, "using", state.universe)

				continue
			}

			state.universe = universe.ID
			wanted[volume.Name] = struct{}{}

			id, added, err := racerctrl.AssignDeviceID(&a.self, volume.Name, space)
			if err != nil {
				a.log.Error("cannot export image volume", "volume", volume.Name, "error", err)

				continue
			}

			if added {
				a.log.Info("exporting image volume",
					"volume", volume.Name, "role", role, "device", blockDevicePath(id))
			}

			extent := volume.Composition[0]

			state.members = append(state.members, imageMember{
				Volume:   volume.Name,
				Role:     role,
				ExtentID: extent.ExtentID,
				DeviceID: id,
				Bytes:    extent.Bytes(),
			})
		}
	}

	for name := range a.imageBound {
		if _, ok := wanted[name]; ok {
			continue
		}

		if racerctrl.ReleaseDeviceID(&a.self, name) {
			a.log.Info("no longer exporting image volume", "volume", name)
		}
	}

	changed := len(wanted) != len(a.imageBound)
	if !changed {
		for name := range wanted {
			if _, ok := a.imageBound[name]; !ok {
				changed = true

				break
			}
		}
	}

	a.imageBound = wanted
	a.image = state

	if changed {
		a.saveBindings()
	}
}

// adoptImageGeneration continues the generation sequence of a map an earlier
// run of this agent left behind.
//
// The file lives on a tmpfs, so a reboot takes it with the devices it named and
// starting again at one is correct. A restart of this container alone does not:
// the reader keeps the highest generation it has seen, and a map that went
// backwards would be ignored for as long as the process lived.
func (a *Agent) adoptImageGeneration() {
	if a.cfg.ImageDevicesPath == "" {
		return
	}

	data, err := os.ReadFile(a.cfg.ImageDevicesPath)
	if err != nil {
		return
	}

	set, err := segment.Parse(data)
	if err != nil {
		a.log.Warn("ignoring an unreadable image device map",
			"path", a.cfg.ImageDevicesPath, "error", err)

		return
	}

	a.imageGeneration = set.Generation
}

// imageLoop keeps the published image device map in step with what this node
// actually exports.
func (a *Agent) imageLoop(ctx context.Context) {
	if a.cfg.ImageDevicesPath == "" {
		return
	}

	ticker := time.NewTicker(imagePublishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := a.publishImageDevices(); err != nil {
			a.log.Error("could not publish the image device map",
				"path", a.cfg.ImageDevicesPath, "error", err)
		}
	}
}

// publishImageDevices writes the image device map if it has changed.
//
// A device is only named once its node exists. The file is read by a
// snapshotter that maps what it names with device-mapper, and naming a device
// that is not there yet would turn a race into a failed container start rather
// than into a miss the snapshotter already knows how to handle.
func (a *Agent) publishImageDevices() error {
	a.mu.Lock()
	state := a.image
	a.mu.Unlock()

	set := segment.Set{Universe: state.universe}

	members := append([]imageMember(nil), state.members...)
	sort.Slice(members, func(i, j int) bool { return members[i].ExtentID < members[j].ExtentID })

	for _, member := range members {
		device := blockDevicePath(member.DeviceID)
		if _, err := os.Stat(device); err != nil {
			continue
		}

		switch member.Role {
		case racerctrl.ImageRoleCatalog:
			set.Catalog = segment.Catalog{Device: device, Bytes: member.Bytes}
		case racerctrl.ImageRoleSegment:
			set.Segments = append(set.Segments, segment.Segment{
				ID:     member.ExtentID,
				Device: device,
				Bytes:  member.Bytes,
			})
		}
	}

	// An empty map is not the same as no map. Publishing one would tell the
	// snapshotter this cluster has an image volume with nothing in it, which
	// makes every lookup an error rather than a miss.
	if set.Catalog.Device == "" && len(set.Segments) == 0 {
		return nil
	}

	data, err := json.Marshal(set)
	if err != nil {
		return fmt.Errorf("marshal image device map: %w", err)
	}

	// Round-trip through the consumer's own parser before writing. It rejects
	// unknown fields, so this is what keeps the two ends of a contract that has
	// no schema negotiation from drifting apart silently.
	if _, err := segment.Parse(data); err != nil {
		return fmt.Errorf("refusing to publish an image device map the snapshotter would reject: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if string(data) == string(a.imagePublished) {
		return nil
	}

	// The generation is what tells a reader the file it just re-read is newer
	// than the one it had. It only advances when the content does.
	set.Generation = a.imageGeneration + 1

	stamped, err := json.Marshal(set)
	if err != nil {
		return fmt.Errorf("marshal image device map: %w", err)
	}

	dir := filepath.Dir(a.cfg.ImageDevicesPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	if err := racerctrl.WriteFileAtomic(a.cfg.ImageDevicesPath, stamped); err != nil {
		return fmt.Errorf("write %s: %w", a.cfg.ImageDevicesPath, err)
	}

	a.imagePublished = data
	a.imageGeneration = set.Generation

	a.log.Info("published the image device map",
		"path", a.cfg.ImageDevicesPath,
		"generation", set.Generation,
		"universe", set.Universe,
		"segments", len(set.Segments),
		"catalog", set.Catalog.Device)

	return nil
}
