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

// imageSegment is one IMMUTABLE_4M extent of the image volume as the
// snapshotter needs to see it: where it starts in the device every node
// exports, how big it is, and which tombstone epoch it is on.
type imageSegment struct {
	ExtentID uint32
	Offset   uint64
	Bytes    uint64
	Epoch    uint32
}

// imageMember is the cluster image volume as this node sees it: which of this
// node's ublk minors carries it, and how its one address space is cut up.
//
// The catalog is not listed among the segments. It is the volume's mutable
// head, so it is always the first entry of the composition and therefore always
// at offset zero, and the snapshotter finds it there rather than by name.
type imageMember struct {
	// Volume is the PersistentVolume name, used only for logs.
	Volume string

	// DeviceID is this node's ublk minor for the volume.
	DeviceID uint32

	// CatalogBytes is the size of the mutable head, or zero if the volume has
	// no mutable head, which is a volume the snapshotter cannot use.
	CatalogBytes uint64

	// Segments are the immutable extents, in device order.
	Segments []imageSegment
}

// imageState is everything the publisher needs, captured under the agent's
// mutex so the loop that writes the file never touches agent state.
type imageState struct {
	universe uint32
	member   *imageMember
}

// imageVolumeNames returns every PersistentVolume that declares itself the
// image volume.
//
// The marker lives on the PV rather than on the StorageClass because a universe
// holds ordinary volumes too, and rather than in a CRD because everything else
// about a racer volume is already an annotation on the object it describes.
func imageVolumeNames(volumes []*corev1.PersistentVolume) map[string]struct{} {
	names := map[string]struct{}{}

	for _, pv := range volumes {
		if pv == nil || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != racerctrl.DriverName {
			continue
		}

		if pv.Annotations[racerctrl.ImageRoleAnnotation] == racerctrl.ImageRoleImage {
			names[pv.Name] = struct{}{}
		}
	}

	return names
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
func (a *Agent) reconcileImageVolumes(cluster racerctrl.ClusterState, names map[string]struct{}) {
	if len(names) == 0 && len(a.imageBound) == 0 {
		return
	}

	space := a.minorSpace()
	wanted := map[string]struct{}{}

	state := imageState{}

	for _, universe := range cluster.Universes {
		for i := range universe.Volumes {
			volume := &universe.Volumes[i]
			if _, ok := names[volume.Name]; !ok {
				continue
			}

			if len(volume.Composition) == 0 {
				continue
			}

			// One image volume. The snapshotter addresses a segment by id
			// within a single address space and has nowhere to put a second
			// one, so a second volume is not silently mixed in.
			if state.member != nil {
				a.log.Warn("ignoring a second image volume",
					"volume", volume.Name, "using", state.member.Volume)

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
					"volume", volume.Name, "device", blockDevicePath(id))
			}

			state.member = imageMemberOf(volume, id)
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

// imageMemberOf cuts a volume's composition into the shape the snapshotter
// reads: a mutable head at offset zero, then the immutable extents after it.
//
// The offsets are computed rather than published by the allocator because they
// are a property of the composition, which is stamped once and frozen. Every
// node therefore derives the same numbers from the same annotation, which is
// what lets a catalog record written on one node resolve on another.
func imageMemberOf(volume *racerctrl.VolumeState, device uint32) *imageMember {
	member := &imageMember{Volume: volume.Name, DeviceID: device}

	var offset uint64

	for _, segment := range volume.Composition {
		bytes := segment.Bytes()

		if racerctrl.KindIsImmutable(segment.Kind) {
			member.Segments = append(member.Segments, imageSegment{
				ExtentID: segment.ExtentID,
				Offset:   offset,
				Bytes:    bytes,
				Epoch:    volume.TombstoneEpochs[segment.ExtentID],
			})
		} else if len(member.Segments) == 0 {
			// The catalog is the mutable head and nothing else is mutable. A
			// mutable extent found after a segment would mean a composition
			// the snapshotter cannot read, so it is left out and the volume
			// publishes with a smaller catalog than it has, which fails
			// loudly on the reader rather than quietly here.
			member.CatalogBytes += bytes
		}

		offset += bytes
	}

	return member
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

	// Remember what is on disk, not just how far the sequence got. Without this a
	// restart cannot tell an unchanged map from one it has never written: it would
	// burn a generation republishing identical content, and worse, a node that
	// comes back with no image devices would read its own stale file as "nothing
	// published yet" and leave it there naming minors that have since been reused.
	set.Generation = 0

	unstamped, err := json.Marshal(set)
	if err != nil {
		return
	}

	a.imagePublished = unstamped
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
	// An empty path is what LoadConfig turns RACER_IMAGE_DEVICES="-" into,
	// and it means publication is off. Checked here rather than only in the
	// loop that calls this, because WriteFileAtomic renames into the
	// directory of its target and an empty target names the process's
	// working directory.
	if a.cfg.ImageDevicesPath == "" {
		return nil
	}

	a.mu.Lock()
	state := a.image
	a.mu.Unlock()

	set := segment.Set{Universe: state.universe}

	if member := state.member; member != nil {
		if device := blockDevicePath(member.DeviceID); statable(device) {
			set.Device = device
			set.CatalogBytes = member.CatalogBytes

			for _, seg := range member.Segments {
				set.Segments = append(set.Segments, segment.Segment{
					ID:     seg.ExtentID,
					Offset: seg.Offset,
					Bytes:  seg.Bytes,
					Epoch:  seg.Epoch,
				})
			}
		}
	}

	sort.Slice(set.Segments, func(i, j int) bool { return set.Segments[i].ID < set.Segments[j].ID })

	// An empty map is not the same as no map, so on a node that has never had an
	// image device there is nothing to say and creating the file only invents a
	// worse answer than its absence. Once something has been published the empty
	// map is the only way to take it back: the reader keeps serving the last set
	// it loaded when the file disappears, and the minors named in a stale set are
	// reused for other volumes, so leaving it in place would eventually hand the
	// snapshotter another volume's bytes as if they were layer data.
	empty := set.Device == ""

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

	if empty && a.imagePublished == nil {
		return nil
	}

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
		"device", set.Device)

	return nil
}

// statable reports whether a path exists, which for a block device means the
// node has been created and the device can be opened.
func statable(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
