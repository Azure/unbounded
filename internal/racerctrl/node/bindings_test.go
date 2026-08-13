// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// newBindingsAgent builds the smallest Agent the bindings paths need: a config
// directory, a logger, and the in-memory state. Nothing here talks to the API
// server, because nothing in the binding path does.
func newBindingsAgent(t *testing.T, dir string) *Agent {
	t.Helper()

	return &Agent{
		cfg:    Config{NodeName: "node-a", ConfigDir: dir},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		signal: make(chan struct{}, 1),
		self:   racerctrl.NodeState{Name: "node-a", Live: map[uint32]racerctrl.LiveExtent{}},
	}
}

// restart stands in for the racer-ctrl container being replaced while racer
// keeps running: a fresh agent over the same directory.
func restart(t *testing.T, dir string) *Agent {
	t.Helper()

	next := newBindingsAgent(t, dir)
	if err := next.adoptExistingState(); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	return next
}

func volumeOf(a *Agent, volume string) (uint32, bool) {
	for _, binding := range a.self.Devices {
		if binding.Volume == volume {
			return binding.DeviceID, true
		}
	}

	return 0, false
}

func fabricOf(a *Agent, universe uint32) (uint32, bool) {
	for _, export := range a.self.Fabric {
		if export.UniverseID == universe {
			return export.DeviceID, true
		}
	}

	return 0, false
}

// The blocker this file exists for: racer is a separate container in the same
// pod and does not restart when the agent does, so its exports are still up and
// still in use. An agent that forgot which volume each minor carried would
// render a config with no devices and take them away from running pods.
func TestBindingsSurviveAnAgentRestart(t *testing.T) {
	dir := t.TempDir()

	first := newBindingsAgent(t, dir)

	for _, volume := range []string{"pv-a", "pv-b"} {
		if _, _, err := racerctrl.AssignDeviceID(&first.self, volume, racerctrl.MinorSpace{}); err != nil {
			t.Fatalf("assign %s: %v", volume, err)
		}
	}

	if _, _, err := racerctrl.AssignFabricDeviceID(&first.self, 11, racerctrl.MinorSpace{}); err != nil {
		t.Fatalf("assign fabric: %v", err)
	}

	first.saveBindings()

	second := restart(t, dir)

	for _, volume := range []string{"pv-a", "pv-b"} {
		want, _ := volumeOf(first, volume)

		got, ok := volumeOf(second, volume)
		if !ok {
			t.Fatalf("%s was forgotten across the restart", volume)
		}

		if got != want {
			t.Fatalf("%s came back on minor %d, want the %d racer is exporting it on", volume, got, want)
		}
	}

	wantFabric, _ := fabricOf(first, 11)

	gotFabric, ok := fabricOf(second, 11)
	if !ok {
		t.Fatal("the universe's fabric minor was forgotten across the restart")
	}

	if gotFabric != wantFabric {
		t.Fatalf("universe 11 came back on minor %d, want %d; peers hold namespaces pointing at the old path",
			gotFabric, wantFabric)
	}
}

// A minor is only free if nothing already holds it. Re-deriving from a
// lowest-free scan after a restart that forgot the fabric export would hand a
// device the universe's minor and silently repoint every peer.
func TestAdoptedMinorsAreNotHandedOutAgain(t *testing.T) {
	dir := t.TempDir()

	first := newBindingsAgent(t, dir)

	fabricID, _, err := racerctrl.AssignFabricDeviceID(&first.self, 11, racerctrl.MinorSpace{})
	if err != nil {
		t.Fatalf("assign fabric: %v", err)
	}

	first.saveBindings()

	second := restart(t, dir)

	id, _, err := racerctrl.AssignDeviceID(&second.self, "pv-new", racerctrl.MinorSpace{})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	if id == fabricID {
		t.Fatalf("a new volume took minor %d, which universe 11's namespace is already published on", id)
	}
}

// A pod that has never staged anything has no file, which is not a failure.
func TestAdoptWithNoBindingsFile(t *testing.T) {
	agent := restart(t, t.TempDir())

	if len(agent.self.Devices) != 0 || len(agent.self.Fabric) != 0 {
		t.Fatalf("adopted %d devices and %d exports from an empty directory",
			len(agent.self.Devices), len(agent.self.Fabric))
	}
}

// adoptExistingState creates the directory it watches, so a first start on a
// fresh emptyDir works.
func TestAdoptCreatesTheConfigDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "etc", "racer")

	agent := newBindingsAgent(t, dir)
	if err := agent.adoptExistingState(); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("config directory was not created: %v", err)
	}
}

// A file that cannot be read costs the exports that were up, which is bad. It
// must not wedge the agent, which would cost everything: no status, no
// membership convergence and no way to recover, over a file the next successful
// stage rewrites.
func TestAdoptIgnoresUnreadableBindings(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "not json", data: "{"},
		{name: "wrong shape", data: `{"devices":"nope"}`},
		{name: "device with no minor", data: `{"devices":[{"deviceId":0,"volume":"pv-a"}]}`},
		{name: "device with no volume", data: `{"devices":[{"deviceId":3,"volume":""}]}`},
		{name: "fabric with no minor", data: `{"fabric":[{"universeId":1,"deviceId":0}]}`},
		{name: "fabric with no universe", data: `{"fabric":[{"universeId":0,"deviceId":3}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()

			if err := os.WriteFile(filepath.Join(dir, BindingsFileName), []byte(test.data), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}

			agent := newBindingsAgent(t, dir)
			if err := agent.adoptExistingState(); err != nil {
				t.Fatalf("a bad bindings file must not stop the agent: %v", err)
			}

			if len(agent.self.Devices) != 0 || len(agent.self.Fabric) != 0 {
				t.Fatal("a rejected bindings file must not be partially applied")
			}

			// Recovery is the next stage rewriting the file.
			if _, _, err := racerctrl.AssignDeviceID(&agent.self, "pv-a", racerctrl.MinorSpace{}); err != nil {
				t.Fatalf("assign: %v", err)
			}

			agent.saveBindings()

			if _, ok := volumeOf(restart(t, dir), "pv-a"); !ok {
				t.Fatal("the file was not repaired by the next stage")
			}
		})
	}
}

// An empty file is the visible half of a write that did not finish. It is read
// back as unusable rather than as an empty set of bindings.
func TestAdoptIgnoresAnEmptyBindingsFile(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, BindingsFileName), nil, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := readBindings(filepath.Join(dir, BindingsFileName)); err == nil {
		t.Fatal("an empty bindings file was accepted as an empty set of bindings")
	}
}

func clusterWithVolumes(names ...string) racerctrl.ClusterState {
	universe := racerctrl.UniverseState{Class: "fast", ID: 1}
	for _, name := range names {
		universe.Volumes = append(universe.Volumes, racerctrl.VolumeState{Name: name})
	}

	return racerctrl.ClusterState{Universes: []racerctrl.UniverseState{universe}}
}

// A volume deleted while the agent was down would otherwise fail every render
// for the life of the pod, because the derivation refuses to build a device for
// a volume no storage class carries.
func TestPruneDropsABindingForADeletedVolume(t *testing.T) {
	dir := t.TempDir()

	first := newBindingsAgent(t, dir)

	for _, volume := range []string{"pv-live", "pv-gone"} {
		if _, _, err := racerctrl.AssignDeviceID(&first.self, volume, racerctrl.MinorSpace{}); err != nil {
			t.Fatalf("assign %s: %v", volume, err)
		}
	}

	first.saveBindings()

	second := restart(t, dir)
	second.pruneAdoptedBindings(clusterWithVolumes("pv-live"))

	if _, ok := volumeOf(second, "pv-gone"); ok {
		t.Fatal("kept a binding for a volume the cluster no longer has")
	}

	if _, ok := volumeOf(second, "pv-live"); !ok {
		t.Fatal("dropped a binding for a volume that still exists")
	}

	// The prune is durable: a second restart must not resurrect it.
	if _, ok := volumeOf(restart(t, dir), "pv-gone"); ok {
		t.Fatal("the dropped binding came back from disk")
	}
}

// Only adopted bindings are eligible. One this process made itself may be for a
// PersistentVolume the informer cache has not seen yet, and dropping it would
// un-export a device the kubelet is waiting on.
func TestPruneSparesABindingThisProcessMade(t *testing.T) {
	agent := newBindingsAgent(t, t.TempDir())

	if _, _, err := racerctrl.AssignDeviceID(&agent.self, "pv-fresh", racerctrl.MinorSpace{}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	agent.adopted = map[string]struct{}{"pv-adopted": {}}

	agent.pruneAdoptedBindings(clusterWithVolumes())

	if _, ok := volumeOf(agent, "pv-fresh"); !ok {
		t.Fatal("dropped a binding this process staged, which the informer cache has not caught up with yet")
	}
}

// The check runs once. After it every surviving binding is as trustworthy as
// one this process staged itself, so a volume that has not reached the cache
// yet is never at risk.
func TestPruneRunsOnlyOnce(t *testing.T) {
	dir := t.TempDir()

	first := newBindingsAgent(t, dir)
	if _, _, err := racerctrl.AssignDeviceID(&first.self, "pv-a", racerctrl.MinorSpace{}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	first.saveBindings()

	second := restart(t, dir)
	second.pruneAdoptedBindings(clusterWithVolumes("pv-a"))

	if second.adopted != nil {
		t.Fatal("the adopted set was not cleared after the first pass")
	}

	second.pruneAdoptedBindings(clusterWithVolumes())

	if _, ok := volumeOf(second, "pv-a"); !ok {
		t.Fatal("a second pass dropped a binding that had already been vouched for")
	}
}

// Unstage is the only thing that may forget a binding, and it must forget it on
// disk too or the next restart would re-export a volume the kubelet released.
func TestUnstageForgetsTheBindingOnDisk(t *testing.T) {
	dir := t.TempDir()

	agent := newBindingsAgent(t, dir)
	if _, _, err := racerctrl.AssignDeviceID(&agent.self, "pv-a", racerctrl.MinorSpace{}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	agent.saveBindings()
	agent.Unstage("pv-a")

	if _, ok := volumeOf(restart(t, dir), "pv-a"); ok {
		t.Fatal("an unstaged volume came back from disk")
	}
}

// Unstaging a volume that was adopted must clear it from the adopted set too,
// so a later prune does not reason about a binding that no longer exists.
func TestUnstageClearsTheAdoptedMark(t *testing.T) {
	dir := t.TempDir()

	first := newBindingsAgent(t, dir)
	if _, _, err := racerctrl.AssignDeviceID(&first.self, "pv-a", racerctrl.MinorSpace{}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	first.saveBindings()

	second := restart(t, dir)
	second.Unstage("pv-a")

	if _, ok := second.adopted["pv-a"]; ok {
		t.Fatal("an unstaged volume is still marked as adopted")
	}
}

// The bindings file lives in the directory racer watches. racer's inotify watch
// matches on the config file's name, so a sibling is not a reload trigger - but
// it must not be mistaken for the config either.
func TestBindingsAreNotTheConfigFile(t *testing.T) {
	cfg := Config{ConfigDir: "/etc/racer"}
	if cfg.BindingsPath() == cfg.ConfigPath() {
		t.Fatal("the bindings file and the config file are the same path")
	}
}

// A half-written file read back after a crash would be worse than no file at
// all: it would describe some of the exports and quietly drop the rest.
func TestWriteBindingsIsAtomicAndOrdered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BindingsFileName)

	self := racerctrl.NodeState{
		Devices: []racerctrl.DeviceBinding{
			{DeviceID: 9, Volume: "pv-b"},
			{DeviceID: 2, Volume: "pv-a"},
		},
		Fabric: []racerctrl.FabricExport{
			{UniverseID: 7, DeviceID: 5, NQN: "nqn.example", Addr: "10.0.0.1:4420"},
			{UniverseID: 3, DeviceID: 4},
		},
	}

	if err := writeBindings(path, self); err != nil {
		t.Fatalf("write: %v", err)
	}

	stored, err := readBindings(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(stored.Devices) != 2 || stored.Devices[0].DeviceID != 2 || stored.Devices[1].DeviceID != 9 {
		t.Fatalf("devices were not written in minor order: %+v", stored.Devices)
	}

	if len(stored.Fabric) != 2 || stored.Fabric[0].UniverseID != 3 || stored.Fabric[1].UniverseID != 7 {
		t.Fatalf("fabric exports were not written in universe order: %+v", stored.Fabric)
	}

	// Nothing but the minors is recorded. The NQN and the address are derived
	// by the fabric manager from the target it actually builds, and a stale
	// copy of either would be published to peers as fact.
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read dir: %v", err)
	} else if len(entries) != 1 {
		t.Fatalf("the write left %d files behind, want just the bindings", len(entries))
	}
}

// A config left by an earlier run is still what racer is serving, so its
// generation is adopted: R1 requires the generation to strictly increase for
// the life of the node, and starting again at one would have every subsequent
// config rejected by a racer that had not restarted with it.
func TestAdoptTakesTheGenerationFromTheExistingConfig(t *testing.T) {
	dir := t.TempDir()

	agent := newBindingsAgent(t, dir)
	if err := agent.adoptExistingState(); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if agent.generation != 0 {
		t.Fatalf("a node with no config starts at generation %d, want 0", agent.generation)
	}

	if err := os.WriteFile(filepath.Join(dir, racerctrl.ConfigFileName), []byte("not a config"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// An unreadable config is not fatal either: the next render replaces it
	// wholesale, and racer could not have loaded it either.
	if generation := restart(t, dir).generation; generation != 0 {
		t.Fatalf("an unreadable config was adopted at generation %d", generation)
	}
}

func TestMinorSpaceAsksTheKernelWhichMinorsAreTaken(t *testing.T) {
	// The character device is created at CMD_ADD_DEV, before the block device
	// is started, so it is the earliest evidence a minor is spoken for.
	dir := t.TempDir()

	previous := devRoot
	devRoot = dir

	t.Cleanup(func() { devRoot = previous })

	for _, name := range []string{"ublkc1", "ublkc2", "ublkb1"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	agent := newBindingsAgent(t, dir)
	space := agent.minorSpace()

	if !space.InUse(1) || !space.InUse(2) {
		t.Fatalf("minors with a character device were reported free")
	}

	if space.InUse(3) {
		t.Fatalf("a minor with no character device was reported taken")
	}

	id, _, err := racerctrl.AssignDeviceID(&agent.self, "pv-a", space)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	if id != 3 {
		t.Fatalf("allocated minor %d, want the lowest one the kernel does not hold (3)", id)
	}
}

// TestDeviceIDBaseIsDerivedFromTheNodeID covers the arrangement where several
// agents share one kernel. Minors are global to the kernel, so a fixed floor
// puts every agent on the same first minor; deriving the floor from the node id
// the operator allocated gives each of them a window nobody else can reach.
func TestDeviceIDBaseIsDerivedFromTheNodeID(t *testing.T) {
	a := newBindingsAgent(t, t.TempDir())

	if got := a.deviceIDBase(); got != 0 {
		t.Fatalf("an unconfigured agent asked for base %d, want the default floor", got)
	}

	a.cfg.DeviceIDBase = 900
	if got := a.deviceIDBase(); got != 900 {
		t.Fatalf("base %d, want the configured 900", got)
	}

	a.cfg.DeriveDeviceIDBase = true

	// Without an identity there is nothing to derive from, so the configured
	// value has to stand rather than collapsing to minor one.
	if got := a.deviceIDBase(); got != 900 {
		t.Fatalf("base %d before the node had an id, want the configured 900", got)
	}

	a.self.ID = 1
	if got := a.deviceIDBase(); got != racerctrl.MinDeviceID {
		t.Fatalf("node 1 got base %d, want %d", got, racerctrl.MinDeviceID)
	}

	a.self.ID = 4

	want := uint32(3*racerctrl.MaxExports + racerctrl.MinDeviceID)
	if got := a.deviceIDBase(); got != want {
		t.Fatalf("node 4 got base %d, want %d", got, want)
	}
}

// TestDerivedWindowsDoNotOverlap is the property the derivation exists for.
func TestDerivedWindowsDoNotOverlap(t *testing.T) {
	seen := map[uint32]uint32{}

	for id := uint32(1); id <= 9; id++ {
		a := newBindingsAgent(t, t.TempDir())
		a.cfg.DeriveDeviceIDBase = true
		a.self.ID = id

		space := a.minorSpace()

		first, last := space.Base, space.Base+racerctrl.MaxExports-1
		for minor := first; minor <= last; minor++ {
			if other, ok := seen[minor]; ok {
				t.Fatalf("node %d and node %d both claim minor %d", other, id, minor)
			}

			seen[minor] = id
		}
	}
}
