// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	racerconfig "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
	"github.com/Azure/unbounded/internal/racerctrl"
)

// newImageAgent builds an agent whose device tree and published map both live
// under a directory the test controls.
func newImageAgent(t *testing.T, dir string) *Agent {
	t.Helper()

	dev := filepath.Join(dir, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatalf("mkdir dev: %v", err)
	}

	previous := devRoot
	devRoot = dev

	t.Cleanup(func() { devRoot = previous })

	agent := newBindingsAgent(t, dir)
	agent.cfg.ImageDevicesPath = filepath.Join(dir, "run", "image-devices.json")
	agent.imageBound = map[string]struct{}{}

	return agent
}

// present creates the block device node racer would have created once it acted
// on a config naming the minor.
func present(t *testing.T, id uint32) {
	t.Helper()

	path := blockDevicePath(id)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

func imagePV(name, role string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{racerctrl.ImageRoleAnnotation: role},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: racerctrl.DriverName},
			},
		},
	}
}

// imageCluster is a universe holding one catalog volume and n segments, each
// allocated as the single extent the publisher requires.
func imageCluster(universe uint32, segments int) racerctrl.ClusterState {
	state := racerctrl.UniverseState{Class: "racer", ID: universe, Epoch: 1}

	state.Volumes = append(state.Volumes, racerctrl.VolumeState{
		Name: "gantry-image-catalog",
		Composition: racerctrl.Composition{{
			ExtentID: 1,
			Pages:    64,
			Kind:     racerconfig.Kind_OCC,
		}},
	})

	for i := range segments {
		state.Volumes = append(state.Volumes, racerctrl.VolumeState{
			Name: fmt.Sprintf("gantry-image-segment-%d", i),
			Composition: racerctrl.Composition{{
				ExtentID: uint32(10 + i),
				Pages:    2,
				Kind:     racerconfig.Kind_IMMUTABLE_4M,
			}},
		})
	}

	return racerctrl.ClusterState{Universes: []racerctrl.UniverseState{state}}
}

func readSet(t *testing.T, path string) segment.Set {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	set, err := segment.Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	return *set
}

func TestImageVolumeRoles(t *testing.T) {
	other := imagePV("ordinary", "")
	delete(other.Annotations, racerctrl.ImageRoleAnnotation)

	foreign := imagePV("foreign", racerctrl.ImageRoleSegment)
	foreign.Spec.CSI.Driver = "disk.csi.azure.com"

	unknown := imagePV("unknown", "something-else")

	noCSI := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "nocsi"}}

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("cat", racerctrl.ImageRoleCatalog),
		imagePV("seg", racerctrl.ImageRoleSegment),
		other, foreign, unknown, noCSI, nil,
	})

	want := map[string]string{
		"cat": racerctrl.ImageRoleCatalog,
		"seg": racerctrl.ImageRoleSegment,
	}

	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}

	for name, role := range want {
		if roles[name] != role {
			t.Fatalf("roles[%q] = %q, want %q", name, roles[name], role)
		}
	}
}

// A node exports every image volume without anything having staged it, which is
// what makes the layer store readable everywhere at once.
func TestReconcileImageVolumesBindsEveryVolume(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
		imagePV("gantry-image-segment-1", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(imageCluster(1, 2), roles)

	if len(agent.self.Devices) != 3 {
		t.Fatalf("devices = %v, want three", agent.self.Devices)
	}

	if agent.image.universe != 1 {
		t.Fatalf("universe = %d, want 1", agent.image.universe)
	}

	if len(agent.image.members) != 3 {
		t.Fatalf("members = %v, want three", agent.image.members)
	}

	// A binding is durable: the racer-ctrl container can be replaced without
	// racer losing the device it is already serving.
	next := restart(t, dir)
	if len(next.self.Devices) != 3 {
		t.Fatalf("devices after restart = %v, want three", next.self.Devices)
	}
}

// The device ids are the ones already assigned, so a second pass neither
// reassigns them nor rewrites the bindings file.
func TestReconcileImageVolumesIsStable(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
	})

	cluster := imageCluster(1, 1)
	agent.reconcileImageVolumes(cluster, roles)

	first := append([]racerctrl.DeviceBinding(nil), agent.self.Devices...)

	agent.reconcileImageVolumes(cluster, roles)

	if got := agent.self.Devices; len(got) != len(first) {
		t.Fatalf("devices = %v, want %v", got, first)
	}

	for i := range first {
		if agent.self.Devices[i] != first[i] {
			t.Fatalf("devices = %v, want %v", agent.self.Devices, first)
		}
	}
}

// A volume that leaves the cluster gives its minor back, or the window fills up
// with devices nothing is serving.
func TestReconcileImageVolumesReleasesRemoved(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(imageCluster(1, 1), roles)

	agent.reconcileImageVolumes(racerctrl.ClusterState{}, nil)

	if len(agent.self.Devices) != 0 {
		t.Fatalf("devices = %v, want none", agent.self.Devices)
	}

	if len(agent.image.members) != 0 {
		t.Fatalf("members = %v, want none", agent.image.members)
	}
}

// A volume with no composition yet has no extents to ship, and binding a device
// to it would make every render fail.
func TestReconcileImageVolumesSkipsUnallocated(t *testing.T) {
	agent := newImageAgent(t, t.TempDir())

	cluster := racerctrl.ClusterState{Universes: []racerctrl.UniverseState{{
		Class: "racer",
		ID:    1,
		Volumes: []racerctrl.VolumeState{
			{Name: "gantry-image-segment-0"},
			{Name: "gantry-image-segment-1", Composition: racerctrl.Composition{
				{ExtentID: 1, Pages: 1, Kind: racerconfig.Kind_IMMUTABLE_4M},
				{ExtentID: 2, Pages: 1, Kind: racerconfig.Kind_IMMUTABLE_4M},
			}},
		},
	}}}

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
		imagePV("gantry-image-segment-1", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(cluster, roles)

	if len(agent.self.Devices) != 0 {
		t.Fatalf("devices = %v, want none", agent.self.Devices)
	}
}

// The snapshotter addresses a segment by id in one address space, so a second
// universe's image volumes are refused rather than mixed in.
func TestReconcileImageVolumesRefusesSecondUniverse(t *testing.T) {
	agent := newImageAgent(t, t.TempDir())

	cluster := imageCluster(1, 1)
	cluster.Universes = append(cluster.Universes, racerctrl.UniverseState{
		Class: "racer-fast",
		ID:    2,
		Volumes: []racerctrl.VolumeState{{
			Name:        "gantry-image-segment-9",
			Composition: racerctrl.Composition{{ExtentID: 99, Pages: 1, Kind: racerconfig.Kind_IMMUTABLE_4M}},
		}},
	})

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
		imagePV("gantry-image-segment-9", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(cluster, roles)

	if agent.image.universe != 1 {
		t.Fatalf("universe = %d, want 1", agent.image.universe)
	}

	for _, member := range agent.image.members {
		if member.Volume == "gantry-image-segment-9" {
			t.Fatalf("members = %v, want no volume from the second universe", agent.image.members)
		}
	}
}

// Nothing is published until the devices exist, because the reader maps what
// the file names and a missing device turns a miss into a failed start.
func TestPublishImageDevicesWaitsForDevices(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(imageCluster(1, 1), roles)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := os.Stat(agent.cfg.ImageDevicesPath); !os.IsNotExist(err) {
		t.Fatalf("stat = %v, want the file to be absent", err)
	}

	// The catalog alone is enough to publish: a snapshotter with a catalog and
	// no segments still answers, it just misses.
	present(t, agent.image.members[0].DeviceID)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	set := readSet(t, agent.cfg.ImageDevicesPath)
	if set.Catalog.Device == "" {
		t.Fatalf("set = %+v, want a catalog", set)
	}

	if len(set.Segments) != 0 {
		t.Fatalf("segments = %v, want none until the device exists", set.Segments)
	}
}

func TestPublishImageDevices(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
		imagePV("gantry-image-segment-1", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(imageCluster(7, 2), roles)

	for _, member := range agent.image.members {
		present(t, member.DeviceID)
	}

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	set := readSet(t, agent.cfg.ImageDevicesPath)

	if set.Generation != 1 {
		t.Fatalf("generation = %d, want 1", set.Generation)
	}

	if set.Universe != 7 {
		t.Fatalf("universe = %d, want 7", set.Universe)
	}

	if set.Catalog.Bytes != 64*racerctrl.SmallPage {
		t.Fatalf("catalog bytes = %d, want %d", set.Catalog.Bytes, 64*racerctrl.SmallPage)
	}

	if len(set.Segments) != 2 {
		t.Fatalf("segments = %v, want two", set.Segments)
	}

	// Segments are addressed by extent id, which is what a catalog record
	// carries. The minor is node-local and cannot serve that purpose.
	if set.Segments[0].ID != 10 || set.Segments[1].ID != 11 {
		t.Fatalf("segments = %v, want ids 10 and 11", set.Segments)
	}

	for _, seg := range set.Segments {
		if seg.Bytes != 2*segment.PageBytes {
			t.Fatalf("segment %d bytes = %d, want %d", seg.ID, seg.Bytes, 2*segment.PageBytes)
		}

		if _, err := set.Segment(seg.ID); err != nil {
			t.Fatalf("segment %d: %v", seg.ID, err)
		}
	}

	if _, err := set.CatalogDevice(); err != nil {
		t.Fatalf("catalog device: %v", err)
	}
}

// The generation is what tells a reader the file it just re-read is newer. It
// only moves when the content does, so a poll that finds nothing new does not
// spend one.
func TestPublishImageDevicesGenerationTracksContent(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(imageCluster(1, 1), roles)

	for _, member := range agent.image.members {
		present(t, member.DeviceID)
	}

	for range 3 {
		if err := agent.publishImageDevices(); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	if got := readSet(t, agent.cfg.ImageDevicesPath).Generation; got != 1 {
		t.Fatalf("generation = %d, want 1", got)
	}

	// A new segment is new content.
	roles["gantry-image-segment-1"] = racerctrl.ImageRoleSegment

	agent.reconcileImageVolumes(imageCluster(1, 2), roles)

	for _, member := range agent.image.members {
		present(t, member.DeviceID)
	}

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if got := readSet(t, agent.cfg.ImageDevicesPath).Generation; got != 2 {
		t.Fatalf("generation = %d, want 2", got)
	}
}

// A restart of this container alone must not republish a lower generation: the
// reader keeps the highest it has seen and would ignore the map for as long as
// the process lived.
func TestAdoptImageGenerationContinuesTheSequence(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(imageCluster(1, 1), roles)

	for _, member := range agent.image.members {
		present(t, member.DeviceID)
	}

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	next := newImageAgent(t, dir)
	if err := next.adoptExistingState(); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if next.imageGeneration != 1 {
		t.Fatalf("generation = %d, want 1", next.imageGeneration)
	}

	next.reconcileImageVolumes(imageCluster(1, 1), roles)

	for _, member := range next.image.members {
		present(t, member.DeviceID)
	}

	if err := next.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if got := readSet(t, next.cfg.ImageDevicesPath).Generation; got != 2 {
		t.Fatalf("generation = %d, want 2", got)
	}
}

// A file left behind by something else is not a reason to refuse to publish.
func TestAdoptImageGenerationIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	if err := os.MkdirAll(filepath.Dir(agent.cfg.ImageDevicesPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(agent.cfg.ImageDevicesPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	agent.adoptImageGeneration()

	if agent.imageGeneration != 0 {
		t.Fatalf("generation = %d, want 0", agent.imageGeneration)
	}
}

// The reader rejects unknown fields, so a map this side would happily marshal
// but that side would refuse must not reach the file.
func TestPublishedMapRoundTripsThroughTheReader(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	roles := imageVolumeRoles([]*corev1.PersistentVolume{
		imagePV("gantry-image-catalog", racerctrl.ImageRoleCatalog),
		imagePV("gantry-image-segment-0", racerctrl.ImageRoleSegment),
	})

	agent.reconcileImageVolumes(imageCluster(1, 1), roles)

	for _, member := range agent.image.members {
		present(t, member.DeviceID)
	}

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	data, err := os.ReadFile(agent.cfg.ImageDevicesPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"generation", "universe", "catalog", "segments"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("published map %v is missing %q", raw, key)
		}
	}

	if len(raw) != 4 {
		t.Fatalf("published map %v carries a field the reader does not know", raw)
	}
}

// An agent that was never told where to publish does not publish, and does not
// fail either: the image volume is optional.
func TestPublishImageDevicesDisabled(t *testing.T) {
	agent := newImageAgent(t, t.TempDir())
	agent.cfg.ImageDevicesPath = ""

	agent.adoptImageGeneration()

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}
}
