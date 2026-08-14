// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	racerconfig "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
	"github.com/Azure/unbounded/internal/racerctrl"
)

// catalogPages is the image volume's OCC head in 4 KiB pages. It is a whole
// 4 MiB because the immutable extents that follow it have to start on a 4 MiB
// boundary, which is the same rule ParseGeometry enforces on the way in.
const catalogPages = racerctrl.HugePage / racerctrl.SmallPage

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

// imageVolume is the one volume the whole cluster shares: an OCC catalog head
// followed by n IMMUTABLE_4M extents, which is the shape ParseGeometry emits.
func imageVolume(segments int) racerctrl.VolumeState {
	volume := racerctrl.VolumeState{
		Name: "gantry-image",
		Composition: racerctrl.Composition{{
			ExtentID: 1,
			Pages:    catalogPages,
			Kind:     racerconfig.Kind_OCC,
		}},
	}

	for i := range segments {
		volume.Composition = append(volume.Composition, racerctrl.Segment{
			ExtentID: uint32(10 + i), //nolint:gosec // small test fixture
			Pages:    2,
			Kind:     racerconfig.Kind_IMMUTABLE_4M,
		})
	}

	return volume
}

func imageCluster(universe uint32, segments int) racerctrl.ClusterState {
	state := racerctrl.UniverseState{Class: "racer", ID: universe, Epoch: 1}
	state.Volumes = append(state.Volumes, imageVolume(segments))

	return racerctrl.ClusterState{Universes: []racerctrl.UniverseState{state}}
}

// imageNames is the selector result for a cluster carrying the one volume.
func imageNames() map[string]struct{} {
	return imageVolumeNames([]*corev1.PersistentVolume{
		imagePV("gantry-image", racerctrl.ImageRoleImage),
	})
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

func TestImageVolumeNames(t *testing.T) {
	other := imagePV("ordinary", "")
	delete(other.Annotations, racerctrl.ImageRoleAnnotation)

	foreign := imagePV("foreign", racerctrl.ImageRoleImage)
	foreign.Spec.CSI.Driver = "disk.csi.azure.com"

	unknown := imagePV("unknown", "something-else")

	noCSI := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "nocsi"}}

	names := imageVolumeNames([]*corev1.PersistentVolume{
		imagePV("gantry-image", racerctrl.ImageRoleImage),
		other, foreign, unknown, noCSI, nil,
	})

	if len(names) != 1 {
		t.Fatalf("names = %v, want just the image volume", names)
	}

	if _, ok := names["gantry-image"]; !ok {
		t.Fatalf("names = %v, want gantry-image", names)
	}
}

func TestReconcileImageVolumesBindsTheVolume(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	agent.reconcileImageVolumes(imageCluster(1, 2), imageNames())

	if agent.image.member == nil {
		t.Fatal("member = nil, want the image volume bound")
	}

	// One volume is one device, however many extents it holds. That is the
	// whole point of folding the segments into a single composition.
	if len(agent.self.Devices) != 1 {
		t.Fatalf("devices = %v, want exactly one", agent.self.Devices)
	}

	member := agent.image.member
	if member.CatalogBytes != catalogPages*racerctrl.SmallPage {
		t.Fatalf("catalog bytes = %d, want %d", member.CatalogBytes, catalogPages*racerctrl.SmallPage)
	}

	if len(member.Segments) != 2 {
		t.Fatalf("segments = %v, want two", member.Segments)
	}

	// Offsets are derived from the composition, so the catalog head comes
	// first and each segment starts where the one before it ended.
	if member.Segments[0].Offset != racerctrl.HugePage {
		t.Fatalf("first segment offset = %d, want %d", member.Segments[0].Offset, racerctrl.HugePage)
	}

	if want := racerctrl.HugePage + 2*segment.PageBytes; member.Segments[1].Offset != uint64(want) {
		t.Fatalf("second segment offset = %d, want %d", member.Segments[1].Offset, want)
	}
}

func TestReconcileImageVolumesIsStable(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	cluster, names := imageCluster(1, 2), imageNames()

	agent.reconcileImageVolumes(cluster, names)

	first := agent.image.member.DeviceID

	agent.reconcileImageVolumes(cluster, names)

	if agent.image.member.DeviceID != first {
		t.Fatalf("device = %d, want it to stay %d", agent.image.member.DeviceID, first)
	}

	if len(agent.self.Devices) != 1 {
		t.Fatalf("devices = %v, want the binding not to be duplicated", agent.self.Devices)
	}
}

func TestReconcileImageVolumesReleasesRemoved(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	agent.reconcileImageVolumes(imageCluster(1, 1), imageNames())

	if len(agent.self.Devices) != 1 {
		t.Fatalf("devices = %v, want one", agent.self.Devices)
	}

	agent.reconcileImageVolumes(racerctrl.ClusterState{}, nil)

	if len(agent.self.Devices) != 0 {
		t.Fatalf("devices = %v, want the minor released", agent.self.Devices)
	}

	if agent.image.member != nil {
		t.Fatalf("member = %+v, want nothing bound", agent.image.member)
	}
}

// A volume the allocator has not placed carries no composition, so there is no
// address space to export and nothing to bind a minor to.
func TestReconcileImageVolumesSkipsUnallocated(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	cluster := imageCluster(1, 1)
	cluster.Universes[0].Volumes[0].Composition = nil

	agent.reconcileImageVolumes(cluster, imageNames())

	if agent.image.member != nil {
		t.Fatalf("member = %+v, want nothing bound", agent.image.member)
	}

	if len(agent.self.Devices) != 0 {
		t.Fatalf("devices = %v, want none", agent.self.Devices)
	}
}

// The snapshotter addresses a segment by id inside one address space, so a
// second image volume has nowhere to go and is refused rather than mixed in.
func TestReconcileImageVolumesRefusesASecondVolume(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	cluster := imageCluster(1, 1)

	second := imageVolume(1)
	second.Name = "gantry-image-two"
	second.Composition[0].ExtentID = 100
	second.Composition[1].ExtentID = 110

	cluster.Universes[0].Volumes = append(cluster.Universes[0].Volumes, second)

	names := imageVolumeNames([]*corev1.PersistentVolume{
		imagePV("gantry-image", racerctrl.ImageRoleImage),
		imagePV("gantry-image-two", racerctrl.ImageRoleImage),
	})

	agent.reconcileImageVolumes(cluster, names)

	if agent.image.member == nil || agent.image.member.Volume != "gantry-image" {
		t.Fatalf("member = %+v, want only the first volume", agent.image.member)
	}

	if len(agent.self.Devices) != 1 {
		t.Fatalf("devices = %v, want only the first volume exported", agent.self.Devices)
	}
}

// A device only exists once racer has acted on the config naming it, so the
// publisher waits rather than naming a path that is not there.
func TestPublishImageDevicesWaitsForTheDevice(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	agent.reconcileImageVolumes(imageCluster(1, 1), imageNames())

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := os.Stat(agent.cfg.ImageDevicesPath); !os.IsNotExist(err) {
		t.Fatalf("stat = %v, want the file to be absent", err)
	}

	present(t, agent.image.member.DeviceID)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if set := readSet(t, agent.cfg.ImageDevicesPath); set.Device == "" {
		t.Fatalf("set = %+v, want a device", set)
	}
}

func TestPublishImageDevices(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	agent.reconcileImageVolumes(imageCluster(7, 2), imageNames())
	present(t, agent.image.member.DeviceID)

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

	if set.CatalogBytes != catalogPages*racerctrl.SmallPage {
		t.Fatalf("catalog bytes = %d, want %d", set.CatalogBytes, catalogPages*racerctrl.SmallPage)
	}

	if len(set.Segments) != 2 {
		t.Fatalf("segments = %v, want two", set.Segments)
	}

	// Segments are addressed by extent id, which is what a catalog record
	// carries. The offset is what turns that into bytes on this node.
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

// The epoch is the only evidence a reclaim finished, so it has to survive the
// trip through the published map.
func TestPublishImageDevicesCarriesTheTombstoneEpoch(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	cluster := imageCluster(1, 2)
	cluster.Universes[0].Volumes[0].TombstoneEpochs = map[uint32]uint32{10: 3}

	agent.reconcileImageVolumes(cluster, imageNames())
	present(t, agent.image.member.DeviceID)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	set := readSet(t, agent.cfg.ImageDevicesPath)

	if set.Segments[0].Epoch != 3 {
		t.Fatalf("segment 10 epoch = %d, want 3", set.Segments[0].Epoch)
	}

	if set.Segments[1].Epoch != 0 {
		t.Fatalf("segment 11 epoch = %d, want 0", set.Segments[1].Epoch)
	}
}

// The generation is what tells a reader the file it just re-read is newer. It
// only moves when the content does, so a poll that finds nothing new does not
// spend one.
func TestPublishImageDevicesGenerationTracksContent(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	cluster, names := imageCluster(1, 1), imageNames()

	agent.reconcileImageVolumes(cluster, names)
	present(t, agent.image.member.DeviceID)

	for range 3 {
		if err := agent.publishImageDevices(); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	if set := readSet(t, agent.cfg.ImageDevicesPath); set.Generation != 1 {
		t.Fatalf("generation = %d, want the repeats to be free", set.Generation)
	}

	agent.reconcileImageVolumes(imageCluster(1, 2), names)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	set := readSet(t, agent.cfg.ImageDevicesPath)
	if set.Generation != 2 {
		t.Fatalf("generation = %d, want 2 once the content changed", set.Generation)
	}

	if len(set.Segments) != 2 {
		t.Fatalf("segments = %v, want two", set.Segments)
	}
}

// A restart must neither burn a generation nor mistake the file it wrote last
// time for something a reader has not seen.
func TestAdoptImageGenerationContinuesTheSequence(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	cluster, names := imageCluster(1, 1), imageNames()

	agent.reconcileImageVolumes(cluster, names)
	present(t, agent.image.member.DeviceID)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	restarted := newImageAgent(t, dir)
	restarted.cfg.ImageDevicesPath = agent.cfg.ImageDevicesPath
	restarted.adoptImageGeneration()

	if restarted.imageGeneration != 1 {
		t.Fatalf("generation = %d, want the sequence to continue at 1", restarted.imageGeneration)
	}

	restarted.reconcileImageVolumes(cluster, names)
	present(t, restarted.image.member.DeviceID)

	if err := restarted.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if set := readSet(t, restarted.cfg.ImageDevicesPath); set.Generation != 1 {
		t.Fatalf("generation = %d, want an unchanged map to cost nothing", set.Generation)
	}
}

// An empty map is not the same as no map: a reader holding a stale set has to
// be told the devices it names are gone, because those minors get reused.
func TestPublishRetractsAMapWhenTheDeviceGoesAway(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	agent.reconcileImageVolumes(imageCluster(1, 1), imageNames())
	present(t, agent.image.member.DeviceID)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	agent.reconcileImageVolumes(racerctrl.ClusterState{}, nil)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	set := readSet(t, agent.cfg.ImageDevicesPath)

	if set.Generation != 2 {
		t.Fatalf("generation = %d, want the retraction to spend one", set.Generation)
	}

	if set.Device != "" || len(set.Segments) != 0 {
		t.Fatalf("set = %+v, want it emptied", set)
	}
}

func TestPublishWritesNothingBeforeAnyDeviceExists(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := os.Stat(agent.cfg.ImageDevicesPath); !os.IsNotExist(err) {
		t.Fatalf("stat = %v, want no file at all", err)
	}
}

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

	if agent.imageGeneration != 0 || agent.imagePublished != nil {
		t.Fatalf("generation = %d, published = %v, want a fresh start",
			agent.imageGeneration, agent.imagePublished)
	}
}

// The producer and the consumer of the map are in different binaries, and the
// reader rejects unknown fields, so this is the test that keeps them together.
func TestPublishedMapRoundTripsThroughTheReader(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	agent.reconcileImageVolumes(imageCluster(4, 2), imageNames())
	present(t, agent.image.member.DeviceID)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	set, err := segment.Load(agent.cfg.ImageDevicesPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	addr := segment.Address{Segment: 11, PageOffset: 1, PageCount: 1, ByteLength: 4096}

	device, offset, length, err := set.Locate(addr)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}

	if device != set.Device {
		t.Fatalf("device = %q, want %q", device, set.Device)
	}

	// The second segment starts past the catalog head and the first segment,
	// and the address adds its own page offset on top of that.
	want := uint64(racerctrl.HugePage) + 2*segment.PageBytes + segment.PageBytes
	if offset != want {
		t.Fatalf("offset = %d, want %d", offset, want)
	}

	if length != segment.PageBytes {
		t.Fatalf("length = %d, want %d", length, segment.PageBytes)
	}

	// The catalog is the same device at offset zero, which is what makes the
	// holder able to open one file for both.
	catalog, err := set.CatalogDevice()
	if err != nil {
		t.Fatalf("catalog device: %v", err)
	}

	if catalog.Device != set.Device {
		t.Fatalf("catalog device = %q, want %q", catalog.Device, set.Device)
	}
}

func TestPublishImageDevicesDisabled(t *testing.T) {
	dir := t.TempDir()
	agent := newImageAgent(t, dir)

	// LoadConfig turns RACER_IMAGE_DEVICES="-" into an empty path, so an
	// empty path is what "publication is off" looks like by the time the
	// agent sees it. Setting the literal "-" here would only prove that a
	// file called "-" can be written.
	agent.cfg.ImageDevicesPath = ""

	agent.reconcileImageVolumes(imageCluster(1, 1), imageNames())
	present(t, agent.image.member.DeviceID)

	if err := agent.publishImageDevices(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	for _, entry := range entries {
		if entry.Name() == "run" {
			t.Fatal("run directory created, want the publisher disabled")
		}
	}
}

func TestLoadConfigTurnsTheOffSwitchIntoAnEmptyPath(t *testing.T) {
	t.Setenv(EnvNodeName, "node-a")
	t.Setenv(EnvImageDevices, ImageDevicesOff)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.ImageDevicesPath != "" {
		t.Fatalf("image devices path = %q, want empty", cfg.ImageDevicesPath)
	}
}

// The map is JSON on a tmpfs, so a hand-written file has to survive the same
// validation the publisher applies to its own output.
func TestParseRejectsAnOverlappingMap(t *testing.T) {
	set := segment.Set{
		Device:       "/dev/ublkb1",
		CatalogBytes: racerctrl.HugePage,
		Segments: []segment.Segment{
			{ID: 10, Offset: racerctrl.HugePage, Bytes: 2 * segment.PageBytes},
			{ID: 11, Offset: racerctrl.HugePage + segment.PageBytes, Bytes: segment.PageBytes},
		},
	}

	data, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := segment.Parse(data); err == nil {
		t.Fatal("parse = nil, want the overlap refused")
	}
}
