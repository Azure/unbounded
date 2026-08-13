//go:build e2e

package racere2e

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racercomponent "github.com/Azure/unbounded/internal/operator/components/racer"
	"github.com/Azure/unbounded/internal/racerctrl"
)

// step is one scenario in the walk. They share a cluster and run in order,
// because racer's control plane is a sequence: nothing about volumes means
// anything until membership has settled, and nothing about membership means
// anything until the nodes have identities.
type step struct {
	name string
	run  func(t *testing.T, h *harness)
}

// TestRacer walks a two-zone racer cluster from an empty kind cluster through
// to a volume that has been written, read, migrated and collected.
func TestRacer(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := h.runOperator(ctx)
	defer stop()

	h.shareKernel(ctx)

	steps := []step{
		{"Identity", stepIdentity},
		{"Workload", stepWorkload},
		{"Universe", stepUniverse},
		{"ConfigLoaded", stepConfigLoaded},
		{"Fabric", stepFabric},
		{"Volume", stepVolume},
		{"Relocate", stepRelocate},
		{"Migrate", stepMigrate},
		{"Collect", stepCollect},
		{"Shrink", stepShrink},
	}

	for _, s := range steps {
		if !t.Run(s.name, func(t *testing.T) { s.run(t, h) }) {
			t.Fatalf("step %q failed; the remaining steps depend on it", s.name)
		}
	}
}

// stepIdentity proves the operator turns nine enrolled but anonymous nodes into
// two racer zones of the shape the annotations asked for.
//
// Zone shape is not cosmetic. A zone's catalog is built from equal cohorts, so
// a six-node zone has to land as 2/2/2 and a three-node zone as 1/1/1 or no
// membership can be planned at all.
func stepIdentity(t *testing.T, h *harness) {
	ctx := context.Background()

	var states map[string]racerctrl.NodeState

	h.waitFor(t, ctx, 3*time.Minute, "every worker to hold a racer identity", func() error {
		var err error

		states, err = h.nodeStates(ctx)
		if err != nil {
			return err
		}

		for _, name := range h.workers {
			state, ok := states[name]
			if !ok {
				return fmt.Errorf("%s is not in the node list", name)
			}

			if state.ID == 0 || state.Zone == 0 {
				return fmt.Errorf("%s has no identity yet (id %d, zone %d)", name, state.ID, state.Zone)
			}
		}

		return nil
	})

	ids := map[uint32]string{}
	zones := map[uint32][]string{}

	for _, name := range h.workers {
		state := states[name]

		if other, clash := ids[state.ID]; clash {
			t.Fatalf("node id %d was handed to both %s and %s", state.ID, other, name)
		}

		ids[state.ID] = name
		zones[state.Zone] = append(zones[state.Zone], name)
	}

	if len(zones) != 2 {
		t.Fatalf("expected two racer zones, got %d: %v", len(zones), zones)
	}

	// The zone ids are minted by the operator, so identify the zones by the
	// annotation that asked for them rather than by number.
	alpha, beta := h.zoneIDs(t, states)

	if got := len(zones[alpha]); got != alphaWorkers {
		t.Fatalf("zone %s (%d) holds %d nodes, want %d", alphaZoneName, alpha, got, alphaWorkers)
	}

	if got := len(zones[beta]); got != betaWorkers {
		t.Fatalf("zone %s (%d) holds %d nodes, want %d", betaZoneName, beta, got, betaWorkers)
	}

	for zone, want := range map[uint32]int{alpha: alphaWorkers / racerctrl.Cohorts, beta: betaWorkers / racerctrl.Cohorts} {
		counts := map[uint32]int{}

		for _, name := range zones[zone] {
			counts[states[name].Cohort]++
		}

		for cohort := range uint32(racerctrl.Cohorts) {
			if counts[cohort] != want {
				t.Fatalf("zone %d cohort %d holds %d nodes, want %d (cohorts must be equal or no catalog can be built)",
					zone, cohort, counts[cohort], want)
			}
		}
	}

	t.Logf("zone %s is %d, zone %s is %d", alphaZoneName, alpha, betaZoneName, beta)
}

// stepWorkload proves the DaemonSet the operator installs actually runs. This
// is the first step that exercises the node containers rather than the API
// server, so it is where preflight failures surface: a node whose store
// filesystem refuses O_DIRECT, or a kernel without enough ublk minors, never
// gets past the init container.
func stepWorkload(t *testing.T, h *harness) {
	ctx := context.Background()

	h.waitFor(t, ctx, 8*time.Minute, "the racer DaemonSet to be ready on every worker", func() error {
		set := &appsv1.DaemonSet{}
		if err := h.cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "racer"}, set); err != nil {
			return err
		}

		if set.Status.DesiredNumberScheduled != workerCount {
			return fmt.Errorf("scheduled on %d nodes, want %d", set.Status.DesiredNumberScheduled, workerCount)
		}

		if set.Status.NumberReady != workerCount {
			return fmt.Errorf("%d of %d pods ready", set.Status.NumberReady, workerCount)
		}

		return nil
	})
}

// stepUniverse proves the default universe exists and both zones have reached
// full membership with a catalog.
//
// Membership is planned one move at a time - the whole point of R6 is that a
// group never loses two of its three members at once - so a zone coming up from
// nothing is several generations of work, and this step is where that
// choreography either converges or stalls.
func stepUniverse(t *testing.T, h *harness) {
	ctx := context.Background()

	var universe uint32

	h.waitFor(t, ctx, 2*time.Minute, "the default universe to be allocated", func() error {
		class, err := h.defaultClass(ctx)
		if err != nil {
			return err
		}

		state, err := racerctrl.ParseUniverseState(class.Name, class.Annotations)
		if err != nil {
			return err
		}

		if state.ID == 0 {
			return fmt.Errorf("storage class %s has no universe id yet", class.Name)
		}

		universe = state.ID

		return nil
	})

	t.Logf("universe %d", universe)

	states, err := h.nodeStates(ctx)
	if err != nil {
		t.Fatalf("read node states: %v", err)
	}

	alpha, beta := h.zoneIDs(t, states)

	for zone, want := range map[uint32]int{alpha: alphaWorkers, beta: betaWorkers} {
		h.waitFor(t, ctx, 10*time.Minute, fmt.Sprintf("zone %d to admit all %d of its nodes", zone, want), func() error {
			members, catalog, err := h.membership(ctx, universe, zone)
			if err != nil {
				return err
			}

			if len(members) != want {
				return fmt.Errorf("zone %d holds %d members, want %d", zone, len(members), want)
			}

			if len(catalog) == 0 {
				return fmt.Errorf("zone %d has no catalog", zone)
			}

			return nil
		})

		members, catalog, err := h.membership(ctx, universe, zone)
		if err != nil {
			t.Fatalf("read membership: %v", err)
		}

		if len(catalog) != catalogSize {
			t.Fatalf("zone %d catalog is %d groups, want %d", zone, len(catalog), catalogSize)
		}

		// Every group must be a trio drawn one node per cohort, which is what
		// makes a quorum of two survive the loss of any single cohort.
		for i, trio := range catalog {
			seen := map[uint32]bool{}

			for cohort, id := range trio {
				if id == 0 {
					t.Fatalf("zone %d group %d cohort %d is empty", zone, i, cohort)
				}

				if seen[id] {
					t.Fatalf("zone %d group %d names node %d twice", zone, i, id)
				}

				seen[id] = true
			}
		}

		t.Logf("zone %d: %d members, %d catalog groups", zone, len(members), len(catalog))
	}
}

// stepConfigLoaded proves the loop closes: the agent published a config, the
// dataplane accepted it, and the agent read that acceptance back out of racer's
// metrics. Until this holds every sequencing gate in the control plane is shut,
// so it is the single most load-bearing assertion in the suite.
func stepConfigLoaded(t *testing.T, h *harness) {
	ctx := context.Background()

	h.waitFor(t, ctx, 5*time.Minute, "every node to report a loaded config", func() error {
		states, err := h.nodeStates(ctx)
		if err != nil {
			return err
		}

		for _, name := range h.workers {
			state := states[name]

			if gate := racerctrl.ConfigLoaded(state); !gate.OK {
				return fmt.Errorf("%s: %s", name, gate.Reason)
			}
		}

		return nil
	})

	states, err := h.nodeStates(ctx)
	if err != nil {
		t.Fatalf("read node states: %v", err)
	}

	for _, name := range h.workers {
		state := states[name]

		if state.Health.RejectedTotal != 0 {
			t.Fatalf("%s: racer rejected %d configs", name, state.Health.RejectedTotal)
		}

		if state.StoreBytes == 0 {
			t.Fatalf("%s: no store size reported", name)
		}

		t.Logf("%s: node %d zone %d cohort %d generation %d store %d MiB",
			name, state.ID, state.Zone, state.Cohort, state.Applied.Generation, state.StoreBytes>>20)
	}
}

// stepFabric proves the NVMe-oF layer came up: every node exports its universe
// namespace as an nvmet subsystem, and every node has connected to the peers
// its config names.
//
// On a shared kernel this is also the step that would catch two agents fighting
// over one nvmet port, because the subsystems would all end up advertised on
// one node's address and the attachments would land on the wrong target.
func stepFabric(t *testing.T, h *harness) {
	ctx := context.Background()

	var universe uint32

	class, err := h.defaultClass(ctx)
	if err != nil {
		t.Fatalf("read the default class: %v", err)
	}

	state, err := racerctrl.ParseUniverseState(class.Name, class.Annotations)
	if err != nil {
		t.Fatalf("parse universe state: %v", err)
	}

	universe = state.ID

	h.waitFor(t, ctx, 5*time.Minute, "every node to export its universe on the fabric", func() error {
		states, err := h.nodeStates(ctx)
		if err != nil {
			return err
		}

		for _, name := range h.workers {
			node := states[name]

			var found bool

			for _, export := range node.Fabric {
				if export.UniverseID == universe {
					found = true

					if export.NQN == "" {
						return fmt.Errorf("%s exports universe %d with no NQN", name, universe)
					}

					if export.DeviceID == 0 {
						return fmt.Errorf("%s exports universe %d with no device", name, universe)
					}
				}
			}

			if !found {
				return fmt.Errorf("%s has not exported universe %d yet", name, universe)
			}
		}

		return nil
	})

	states, err := h.nodeStates(ctx)
	if err != nil {
		t.Fatalf("read node states: %v", err)
	}

	// Every node's export must be a distinct subsystem on a distinct device.
	// A collision here is the shared-kernel failure mode.
	nqns := map[string]string{}
	devices := map[uint32]string{}

	for _, name := range h.workers {
		for _, export := range states[name].Fabric {
			if other, clash := nqns[export.NQN]; clash {
				t.Fatalf("%s and %s both claim NQN %s", other, name, export.NQN)
			}

			nqns[export.NQN] = name

			if other, clash := devices[export.DeviceID]; clash {
				t.Fatalf("%s and %s both claim ublk minor %d", other, name, export.DeviceID)
			}

			devices[export.DeviceID] = name
		}
	}

	t.Logf("%d fabric subsystems on %d distinct minors", len(nqns), len(devices))
}

// ---------------------------------------------------------------------------
// cluster readers
// ---------------------------------------------------------------------------

// waitFor polls until the condition stops returning an error, and reports the
// last reason it gave when it runs out of time. Conditions return errors rather
// than booleans so a timeout says what was still missing.
func (h *harness) waitFor(t *testing.T, ctx context.Context, timeout time.Duration, what string, cond func() error) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	var last error

	for {
		last = cond()
		if last == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
		}

		select {
		case <-ctx.Done():
			t.Fatalf("cancelled waiting for %s: %v", what, last)
		case <-time.After(2 * time.Second):
		}
	}
}

// nodeStates reads what the operator and the agents have written onto the Node
// objects. It is the same view the operator's own pass builds, which is what
// makes the sequencing gates reusable from the test.
func (h *harness) nodeStates(ctx context.Context) (map[string]racerctrl.NodeState, error) {
	list := &corev1.NodeList{}
	if err := h.cli.List(ctx, list); err != nil {
		return nil, err
	}

	states := make(map[string]racerctrl.NodeState, len(list.Items))

	for i := range list.Items {
		node := &list.Items[i]

		state, err := racerctrl.ParseNodeState(node.Name, node.Annotations)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", node.Name, err)
		}

		states[node.Name] = state
	}

	return states, nil
}

// zoneIDs maps the two zone names the harness pinned onto the ids the operator
// minted for them.
func (h *harness) zoneIDs(t *testing.T, states map[string]racerctrl.NodeState) (alpha, beta uint32) {
	t.Helper()

	for i, name := range h.workers {
		zone := states[name].Zone
		if zone == 0 {
			continue
		}

		if zoneNameFor(i) == alphaZoneName {
			alpha = zone
		} else {
			beta = zone
		}
	}

	if alpha == 0 || beta == 0 {
		t.Fatalf("could not resolve both zone ids (alpha %d, beta %d)", alpha, beta)
	}

	if alpha == beta {
		t.Fatalf("both zone names resolved to zone %d", alpha)
	}

	return alpha, beta
}

// defaultClass is the StorageClass the operator creates when a site turns racer
// on and no class already names the driver. A class is a universe.
func (h *harness) defaultClass(ctx context.Context) (*storagev1.StorageClass, error) {
	list := &storagev1.StorageClassList{}
	if err := h.cli.List(ctx, list); err != nil {
		return nil, err
	}

	for i := range list.Items {
		if list.Items[i].Provisioner == racerctrl.DriverName {
			return &list.Items[i], nil
		}
	}

	return nil, fmt.Errorf("no storage class names %s", racerctrl.DriverName)
}

// membership reads a zone's published membership and catalog.
func (h *harness) membership(ctx context.Context, universe, zone uint32) (racerctrl.Membership, racerctrl.Catalog, error) {
	cm := &corev1.ConfigMap{}

	key := client.ObjectKey{
		Namespace: namespace,
		Name:      racerctrl.MembershipConfigMapName(universe, zone),
	}

	if err := h.cli.Get(ctx, key, cm); err != nil {
		return nil, nil, err
	}

	members, err := racerctrl.ParseMembership(cm.Data[racerctrl.MembershipDataKey])
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", key.Name, err)
	}

	catalog, err := racerctrl.ParseCatalog(cm.Data[racerctrl.MembershipCatalogKey])
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", key.Name, err)
	}

	return members, catalog, nil
}

// racerPods lists the DaemonSet's pods by node.
func (h *harness) racerPods(ctx context.Context) (map[string]*corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := h.cli.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	pods := map[string]*corev1.Pod{}

	for i := range list.Items {
		pod := &list.Items[i]
		if strings.HasPrefix(pod.Name, "racer-") && pod.Spec.NodeName != "" {
			pods[pod.Spec.NodeName] = pod
		}
	}

	return pods, nil
}

// workersInZone is the harness's own view of which workers were pinned to a
// zone name, in stable order.
func (h *harness) workersInZone(zone string) []string {
	var names []string

	for i, name := range h.workers {
		if zoneNameFor(i) == zone {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	return names
}

// The dataplane scenario constants.
//
// The volume is deliberately small. Store size is a function of how many pages
// a node carries, and a 64 MiB volume replicated three ways across a six-node
// zone leaves every node with tens of megabytes rather than gigabytes, which
// keeps the suite honest about correctness without making it a disk benchmark.
const (
	volumeName = "racer-e2e-alpha"
	claimName  = "racer-e2e-alpha"
	claimNS    = "default"

	// volumeSize is the whole volume, and mutableSize says all of it is a
	// single LWW extent. An immutable extent is written once, which is the
	// wrong shape for a test that overwrites and re-reads.
	volumeSize  = "64Mi"
	mutableSize = "64Mi"

	// consumerDevice is where the kubelet maps the raw block device.
	consumerDevice = "/dev/racerdisk"

	// patternBytes is how much of the volume the test writes. Four mebibytes
	// is a thousand small pages, which is enough for the migration gate to
	// have something to compare live page counts on.
	patternBytes = 4 << 20

	consumerContainer = "consumer"
)

// pattern regenerates the same bytes anywhere, so a digest taken on one node
// can be checked against a digest taken on another without shipping the data
// through the test process.
func patternScript() string {
	return fmt.Sprintf("yes racer-e2e-pattern-0123456789abcdef | head -c %d > /tmp/pattern", patternBytes)
}

// stepVolume proves the whole write path: the operator allocates extents onto a
// PersistentVolume, the CSI node service composes them into a ublk device, and
// the dataplane stores what a pod writes to it.
func stepVolume(t *testing.T, h *harness) {
	ctx := context.Background()

	class, err := h.defaultClass(ctx)
	if err != nil {
		t.Fatalf("default class: %v", err)
	}

	states, err := h.nodeStates(ctx)
	if err != nil {
		t.Fatalf("node states: %v", err)
	}

	alpha, _ := h.zoneIDs(t, states)

	h.createVolume(t, ctx, class.Name)
	h.createClaim(t, ctx, class.Name)

	// The operator allocates before anything mounts, so this is a control
	// plane assertion rather than a race with the kubelet.
	h.waitFor(t, ctx, 3*time.Minute, "the operator to allocate extents for the volume", func() error {
		state, err := h.volumeState(ctx)
		if err != nil {
			return err
		}

		if len(state.Composition) == 0 {
			return fmt.Errorf("no composition yet")
		}

		if state.Zone != alpha {
			return fmt.Errorf("homed in zone %d, want the six-node zone %d", state.Zone, alpha)
		}

		if state.Phase != racerctrl.PhaseActive {
			return fmt.Errorf("phase %q", state.Phase)
		}

		return nil
	})

	h.growStores(t, ctx)

	pod := h.createConsumer(t, ctx, h.workers[0])
	h.waitForPod(t, ctx, 6*time.Minute, pod)

	want := h.digestOfPattern(t, ctx, pod)

	if _, err := h.exec(ctx, pod, fmt.Sprintf(
		"%s && dd if=/tmp/pattern of=%s bs=1M count=%d oflag=direct conv=fsync status=none",
		patternScript(), consumerDevice, patternBytes>>20,
	)); err != nil {
		t.Fatalf("write the pattern: %v", err)
	}

	got := h.digestOfDevice(t, ctx, pod)
	if got != want {
		t.Fatalf("read back %s, wrote %s", got, want)
	}

	t.Logf("wrote and re-read %d MiB through %s on %s", patternBytes>>20, consumerDevice, h.workers[0])
}

// stepRelocate proves the data is in the zone rather than on the node that
// wrote it. The consumer is destroyed and rebuilt on a different node in a
// different cohort, and it has to see the same bytes.
func stepRelocate(t *testing.T, h *harness) {
	ctx := context.Background()

	h.deleteConsumer(t, ctx)

	// workers[0] and workers[3] are both in the six-node zone but land in the
	// same cohort only if placement is broken, so this also crosses a cohort.
	pod := h.createConsumer(t, ctx, h.workers[3])
	h.waitForPod(t, ctx, 6*time.Minute, pod)

	want := h.digestOfPattern(t, ctx, pod)

	got := h.digestOfDevice(t, ctx, pod)
	if got != want {
		t.Fatalf("%s read %s, want %s", h.workers[3], got, want)
	}

	t.Logf("the same bytes are readable from %s", h.workers[3])
}

// stepMigrate moves the volume to the three-node zone while a consumer in the
// six-node zone holds it open, then reads it back from both sides.
//
// The read from the six-node zone after the move is the interesting one: the
// extents no longer live in that zone at all, so every page has to come back
// over the fabric through the destination zone's gateways.
func stepMigrate(t *testing.T, h *harness) {
	ctx := context.Background()

	states, err := h.nodeStates(ctx)
	if err != nil {
		t.Fatalf("node states: %v", err)
	}

	alpha, beta := h.zoneIDs(t, states)

	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:"%d"}}}`, racerctrl.NextZoneAnnotation, beta)

	volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: volumeName}}
	if err := h.cli.Patch(ctx, volume, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("ask for the migration: %v", err)
	}

	// The destination zone has to be told about the extent before it can size a
	// store for it, and it only sizes a store when it starts. So wait for the
	// destination to have loaded a configuration that names the move, then let
	// the stores grow. Without this the push has nowhere to land and the
	// migration never completes.
	state, err := h.volumeState(ctx)
	if err != nil {
		t.Fatalf("volume state: %v", err)
	}

	h.waitFor(t, ctx, 5*time.Minute, "every node to load the migration", func() error {
		states, err := h.nodeStates(ctx)
		if err != nil {
			return err
		}

		for _, worker := range h.workers {
			for _, segment := range state.Composition {
				if states[worker].Applied.Extents[segment.ExtentID].NextZone != beta {
					return fmt.Errorf("%s has not loaded the move of extent %d", worker, segment.ExtentID)
				}
			}
		}

		return nil
	})

	h.growStores(t, ctx)

	h.waitFor(t, ctx, 15*time.Minute, "the volume to finish migrating to the three-node zone", func() error {
		state, err := h.volumeState(ctx)
		if err != nil {
			return err
		}

		if state.NextZone != 0 {
			return fmt.Errorf("still moving towards zone %d", state.NextZone)
		}

		if state.Zone != beta {
			return fmt.Errorf("homed in zone %d, want %d", state.Zone, beta)
		}

		return nil
	})

	pod := consumerName(h.workers[3])

	want := h.digestOfPattern(t, ctx, pod)

	got := h.digestOfDevice(t, ctx, pod)
	if got != want {
		t.Fatalf("gateway read from zone %d returned %s, want %s", alpha, got, want)
	}

	t.Logf("read the volume from zone %d after it moved to zone %d", alpha, beta)

	h.deleteConsumer(t, ctx)

	local := h.createConsumer(t, ctx, h.workers[alphaWorkers])
	h.waitForPod(t, ctx, 6*time.Minute, local)

	if got := h.digestOfDevice(t, ctx, local); got != want {
		t.Fatalf("%s read %s, want %s", h.workers[alphaWorkers], got, want)
	}

	t.Logf("the same bytes are readable from %s in zone %d", h.workers[alphaWorkers], beta)
}

// stepCollect deletes the volume and proves the operator does not let go until
// every carrier reports the extents drained.
func stepCollect(t *testing.T, h *harness) {
	ctx := context.Background()

	h.deleteConsumer(t, ctx)

	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: claimNS}}
	if err := h.cli.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete claim: %v", err)
	}

	volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: volumeName}}
	if err := h.cli.Delete(ctx, volume); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete volume: %v", err)
	}

	// The finalizer is the assertion. It is removed only once every node that
	// carried a segment has reported the extent empty and tombstone-free, so
	// the volume disappearing at all means collection really drained.
	h.waitFor(t, ctx, 15*time.Minute, "the volume to be collected and its finalizer released", func() error {
		got := &corev1.PersistentVolume{}

		err := h.cli.Get(ctx, types.NamespacedName{Name: volumeName}, got)
		if apierrors.IsNotFound(err) {
			return nil
		}

		if err != nil {
			return err
		}

		return fmt.Errorf("still present with phase %q and finalizers %v",
			got.Annotations[racerctrl.PhaseAnnotation], got.Finalizers)
	})
}

// stepShrink takes three nodes out of the six-node zone, one per cohort, and
// waits for the zone to converge on a three-node membership and for the
// retired nodes to lose their identities.
//
// This is the slowest scenario by design: a membership step moves one node at a
// time and each step is a generation the whole zone has to load and quiesce
// before the next one is planned.
func stepShrink(t *testing.T, h *harness) {
	ctx := context.Background()

	states, err := h.nodeStates(ctx)
	if err != nil {
		t.Fatalf("node states: %v", err)
	}

	alpha, _ := h.zoneIDs(t, states)

	class, err := h.defaultClass(ctx)
	if err != nil {
		t.Fatalf("default class: %v", err)
	}

	universe, err := racerctrl.ParseUniverseState(class.Name, class.Annotations)
	if err != nil {
		t.Fatalf("parse universe: %v", err)
	}

	// workers 3, 4 and 5 are the second node of each cohort, so removing them
	// leaves the zone with a legal 1/1/1 shape rather than an unbalanced one.
	retired := []string{h.workers[3], h.workers[4], h.workers[5]}

	for _, name := range retired {
		patch := fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, racercomponent.EnrollmentLabel)

		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := h.cli.Patch(ctx, node, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			t.Fatalf("un-enrol %s: %v", name, err)
		}
	}

	h.waitFor(t, ctx, 25*time.Minute, "the six-node zone to shrink to three members", func() error {
		members, catalog, err := h.membership(ctx, universe.ID, alpha)
		if err != nil {
			return err
		}

		if len(members) != 3 {
			return fmt.Errorf("%d members", len(members))
		}

		if len(catalog) != catalogSize {
			return fmt.Errorf("catalog has %d groups", len(catalog))
		}

		ids := map[uint32]bool{}
		for _, member := range members {
			ids[member.NodeID] = true
		}

		for _, group := range catalog {
			for _, id := range group {
				if !ids[id] {
					return fmt.Errorf("catalog still names node %d", id)
				}
			}
		}

		return nil
	})

	h.waitFor(t, ctx, 10*time.Minute, "the retired nodes to give up their identities", func() error {
		states, err := h.nodeStates(ctx)
		if err != nil {
			return err
		}

		for _, name := range retired {
			if states[name].ID != 0 {
				return fmt.Errorf("%s still holds id %d", name, states[name].ID)
			}
		}

		return nil
	})

	// The survivors have to still be usable, or "shrink" only means "broke".
	h.waitFor(t, ctx, 5*time.Minute, "the surviving nodes to stay healthy", func() error {
		states, err := h.nodeStates(ctx)
		if err != nil {
			return err
		}

		for _, name := range []string{h.workers[0], h.workers[1], h.workers[2]} {
			if gate := racerctrl.ConfigLoaded(states[name]); !gate.OK {
				return fmt.Errorf("%s: %s", name, gate.Reason)
			}
		}

		return nil
	})
}

// consumerName keeps the pod name tied to the node it is pinned to, so a
// relocation cannot accidentally reuse the previous pod.
func consumerName(node string) string {
	return "racer-consumer-" + node
}

// createVolume writes the static PersistentVolume. There is no provisioner in
// this deployment, so the volume is an admin object and the operator's job is
// only to allocate extents onto it.
func (h *harness) createVolume(t *testing.T, ctx context.Context, class string) {
	t.Helper()

	mode := corev1.PersistentVolumeBlock

	volume := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(volumeSize)},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              class,
			VolumeMode:                    &mode,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       racerctrl.DriverName,
					VolumeHandle: volumeName,
					VolumeAttributes: map[string]string{
						"mutableBytes": mutableSize,
						"mutableKind":  "LWW",
					},
				},
			},
		},
	}

	if err := h.cli.Create(ctx, volume); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create volume: %v", err)
	}
}

func (h *harness) createClaim(t *testing.T, ctx context.Context, class string) {
	t.Helper()

	mode := corev1.PersistentVolumeBlock

	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: claimNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			VolumeMode:       &mode,
			VolumeName:       volumeName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(volumeSize)},
			},
		},
	}

	if err := h.cli.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create claim: %v", err)
	}
}

// createConsumer pins a pod to one node and hands it the volume as a raw block
// device. The image is the agent's own, because it is already on every node and
// carries a real coreutils, which matters when the test wants O_DIRECT.
func (h *harness) createConsumer(t *testing.T, ctx context.Context, node string) string {
	t.Helper()

	privileged := true
	name := consumerName(node)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: claimNS},
		Spec: corev1.PodSpec{
			NodeName:      node,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            consumerContainer,
				Image:           imageRef("racer-ctrl"),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/sleep", "infinity"},
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
				VolumeDevices: []corev1.VolumeDevice{{
					Name:       "data",
					DevicePath: consumerDevice,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
				},
			}},
		},
	}

	if err := h.cli.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create consumer on %s: %v", node, err)
	}

	return name
}

// deleteConsumer removes every consumer pod and waits for them to be gone, so
// that the next NodeStageVolume is not racing an unstage on another node.
func (h *harness) deleteConsumer(t *testing.T, ctx context.Context) {
	t.Helper()

	for _, node := range h.workers {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: consumerName(node), Namespace: claimNS}}
		if err := h.cli.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete consumer on %s: %v", node, err)
		}
	}

	h.waitFor(t, ctx, 5*time.Minute, "the consumer pods to go away", func() error {
		pods := &corev1.PodList{}
		if err := h.cli.List(ctx, pods, client.InNamespace(claimNS)); err != nil {
			return err
		}

		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, "racer-consumer-") {
				return fmt.Errorf("%s is still terminating", pods.Items[i].Name)
			}
		}

		return nil
	})
}

func (h *harness) waitForPod(t *testing.T, ctx context.Context, timeout time.Duration, name string) {
	t.Helper()

	h.waitFor(t, ctx, timeout, "pod "+name+" to be running", func() error {
		pod := &corev1.Pod{}
		if err := h.cli.Get(ctx, types.NamespacedName{Namespace: claimNS, Name: name}, pod); err != nil {
			return err
		}

		if pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("phase %s: %s", pod.Status.Phase, podTrouble(pod))
		}

		for _, status := range pod.Status.ContainerStatuses {
			if !status.Ready {
				return fmt.Errorf("container %s is not ready", status.Name)
			}
		}

		return nil
	})
}

// podTrouble summarises why a pod is not running, so a staging failure shows up
// in the test output instead of only in the artifacts.
func podTrouble(pod *corev1.Pod) string {
	var parts []string

	for _, cond := range pod.Status.Conditions {
		if cond.Status != corev1.ConditionTrue && cond.Message != "" {
			parts = append(parts, string(cond.Type)+": "+cond.Message)
		}
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			parts = append(parts, status.Name+": "+status.State.Waiting.Reason)
		}
	}

	if len(parts) == 0 {
		return "no detail"
	}

	return strings.Join(parts, "; ")
}

// exec runs a shell snippet in a consumer pod. It shells out to kubectl rather
// than building a SPDY executor, because the harness already drives the CLI and
// one more dependency here buys nothing.
func (h *harness) exec(ctx context.Context, pod, script string) (string, error) {
	return h.output(ctx, "kubectl", "--kubeconfig", h.kubeconfig,
		"exec", "-n", claimNS, pod, "-c", consumerContainer, "--", "/bin/sh", "-c", script)
}

// digestOfPattern regenerates the reference bytes inside the pod and returns
// their digest, so nothing has to be carried between pods or steps.
func (h *harness) digestOfPattern(t *testing.T, ctx context.Context, pod string) string {
	t.Helper()

	out, err := h.exec(ctx, pod, patternScript()+" && md5sum /tmp/pattern | cut -d' ' -f1")
	if err != nil {
		t.Fatalf("digest the pattern in %s: %v", pod, err)
	}

	return strings.TrimSpace(out)
}

// digestOfDevice reads the written prefix back with O_DIRECT, so the answer
// comes from racer rather than from the page cache.
func (h *harness) digestOfDevice(t *testing.T, ctx context.Context, pod string) string {
	t.Helper()

	script := fmt.Sprintf("dd if=%s bs=1M count=%d iflag=direct status=none | md5sum | cut -d' ' -f1",
		consumerDevice, patternBytes>>20)

	out, err := h.exec(ctx, pod, script)
	if err != nil {
		t.Fatalf("read %s in %s: %v", consumerDevice, pod, err)
	}

	return strings.TrimSpace(out)
}

func (h *harness) volumeState(ctx context.Context) (racerctrl.VolumeState, error) {
	volume := &corev1.PersistentVolume{}
	if err := h.cli.Get(ctx, types.NamespacedName{Name: volumeName}, volume); err != nil {
		return racerctrl.VolumeState{}, err
	}

	return racerctrl.ParseVolumeState(volume.Name, volume.Annotations)
}
