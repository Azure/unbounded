// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"

	"google.golang.org/protobuf/proto"

	racerconfig "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racerctrl"
)

// Loop timings.
const (
	// informerResync bounds how long a missed watch event can go unnoticed.
	informerResync = 5 * time.Minute

	// cacheSyncTimeout fails fast when RBAC is missing rather than hanging
	// forever on a watch that will never be allowed to start.
	cacheSyncTimeout = 2 * time.Minute

	// reconcileDebounce coalesces the burst of events a single logical change
	// produces. A StorageClass edit that admits a node moves annotations on
	// several objects at once and there is no value in rendering four configs
	// on the way to the fifth.
	reconcileDebounce = 200 * time.Millisecond

	// reconcileFloor bounds how often a reconcile can run even under a
	// continuous stream of events, so a hot cluster cannot make this node spend
	// its time rendering protobuf.
	reconcileFloor = time.Second

	// scrapeInterval is how often racer's metrics endpoint is read. The
	// endpoint is unauthenticated plaintext serving one connection at a time,
	// so R8 asks for a modest interval, and none of the sequenced operations
	// that consume it are latency sensitive.
	scrapeInterval = 15 * time.Second

	// scrapeTimeout bounds one scrape.
	scrapeTimeout = 5 * time.Second

	// stagePollInterval is how often NodeStageVolume checks for the device.
	stagePollInterval = 250 * time.Millisecond
)

// Agent is the node half of the racer control plane.
//
// It owns exactly one file, the NodeConfig racer watches, and exactly five
// annotations, the ones on its own Node describing what it is doing. Everything
// else it reads. That division is what makes the whole design safe to run on
// every node at once: two agents can never disagree about a value because no
// value has two writers.
type Agent struct {
	cfg     Config
	log     *slog.Logger
	client  kubernetes.Interface
	fabric  *Fabric
	scraper *Scraper

	factory       informers.SharedInformerFactory
	nodeLister    corelisters.NodeLister
	volumeLister  corelisters.PersistentVolumeLister
	classLister   storagelisters.StorageClassLister
	informersSync []cache.InformerSynced

	// signal is a capacity-one channel used as a coalescing "something
	// changed" flag. A full channel already means a reconcile is pending, so a
	// non-blocking send is exactly the right semantics.
	signal chan struct{}

	mu sync.Mutex

	// self holds the parts of this node's state the agent itself owns: the
	// volumes it exports, the fabric minors it publishes, and what it last
	// scraped. The identity fields are refreshed from the Node object on every
	// reconcile because the operator owns those.
	self racerctrl.NodeState

	// attachments is what the fabric manager most recently achieved.
	attachments map[racerctrl.Attachment]string

	// published is the config racer is currently being served, and generation
	// its generation. R1 requires the generation strictly increase per node, so
	// it is only ever bumped when a genuinely different config is installed.
	published  *racerconfig.NodeConfig
	generation uint64
}

// NewAgent builds an agent. It does not touch the cluster or the host.
func NewAgent(cfg Config, client kubernetes.Interface, log *slog.Logger) *Agent {
	factory := informers.NewSharedInformerFactory(client, informerResync)

	nodes := factory.Core().V1().Nodes()
	volumes := factory.Core().V1().PersistentVolumes()
	classes := factory.Storage().V1().StorageClasses()

	a := &Agent{
		cfg:          cfg,
		log:          log,
		client:       client,
		scraper:      NewScraper(cfg.MetricsURL, scrapeTimeout),
		factory:      factory,
		nodeLister:   nodes.Lister(),
		volumeLister: volumes.Lister(),
		classLister:  classes.Lister(),
		signal:       make(chan struct{}, 1),
		attachments:  map[racerctrl.Attachment]string{},
		self:         racerctrl.NodeState{Name: cfg.NodeName, Live: map[uint32]racerctrl.LiveExtent{}},
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { a.Trigger() },
		UpdateFunc: func(any, any) { a.Trigger() },
		DeleteFunc: func(any) { a.Trigger() },
	}

	for _, informer := range []cache.SharedIndexInformer{
		nodes.Informer(), volumes.Informer(), classes.Informer(),
	} {
		//nolint:errcheck // the handler registration only fails on a stopped informer
		_, _ = informer.AddEventHandler(handler)

		a.informersSync = append(a.informersSync, informer.HasSynced)
	}

	return a
}

// Trigger asks for a reconcile. It never blocks: a pending request already
// covers whatever this one would have done, because every reconcile renders
// whole state from scratch.
func (a *Agent) Trigger() {
	select {
	case a.signal <- struct{}{}:
	default:
	}
}

// Run starts the informers and services reconcile requests until the context is
// cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.adoptExistingConfig(); err != nil {
		return err
	}

	a.factory.Start(ctx.Done())

	syncCtx, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), a.informersSync...) {
		return errors.New("timed out waiting for Node, PersistentVolume and StorageClass caches to sync; " +
			"check the racer-ctrl ClusterRole")
	}

	go a.scrapeLoop(ctx)

	a.Trigger()

	var (
		debounce  <-chan time.Time
		lastRun   time.Time
		debouncer *time.Timer
	)

	stopTimer := func() {
		if debouncer != nil {
			debouncer.Stop()
			debouncer = nil
			debounce = nil
		}
	}

	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-a.signal:
			if debouncer != nil {
				continue
			}

			delay := reconcileDebounce
			if since := time.Since(lastRun); since < reconcileFloor {
				delay = reconcileFloor - since
			}

			debouncer = time.NewTimer(delay)
			debounce = debouncer.C

		case <-debounce:
			stopTimer()

			lastRun = time.Now()

			if err := a.Reconcile(ctx); err != nil {
				// Keep serving the config racer already has. A render that
				// cannot be completed is not a reason to take away the one
				// that works.
				a.log.Error("reconcile failed; keeping the previously published config", "error", err)
			}
		}
	}
}

// adoptExistingConfig reads the config left behind by an earlier run so the
// generation counter continues rather than restarting.
//
// R1 requires the generation strictly increase for the life of a node, and a
// restarted agent that began again at one would have every subsequent config
// rejected by a racer that had not restarted with it.
func (a *Agent) adoptExistingConfig() error {
	if err := os.MkdirAll(a.cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("create config directory %s: %w", a.cfg.ConfigDir, err)
	}

	existing, err := racerctrl.ReadConfig(a.cfg.ConfigPath())
	if err != nil {
		// A corrupt or truncated file is not fatal: the next render replaces
		// it wholesale. Starting the generation from zero is safe in that case
		// precisely because racer could not have loaded it either.
		a.log.Warn("ignoring unreadable existing config", "path", a.cfg.ConfigPath(), "error", err)

		return nil
	}

	if existing == nil {
		return nil
	}

	a.published = existing
	a.generation = existing.GetGeneration()

	// Only the generation is adopted, not the device list. The config records
	// device ids but not the volume names they were staged for, and the
	// kubelet re-issues NodeStageVolume for every volume a running pod holds
	// after a plugin restart, so the bindings are rebuilt from those calls.
	// Until they arrive the node exports nothing, which is correct: nothing is
	// mounted either.
	a.log.Info("adopted existing racer config",
		"path", a.cfg.ConfigPath(), "generation", a.generation)

	return nil
}

// Reconcile renders and installs this node's config once.
func (a *Agent) Reconcile(ctx context.Context) error {
	nodes, err := a.nodeLister.List(labelsEverything())
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	volumes, err := a.volumeLister.List(labelsEverything())
	if err != nil {
		return fmt.Errorf("list persistent volumes: %w", err)
	}

	classes, err := a.classLister.List(labelsEverything())
	if err != nil {
		return fmt.Errorf("list storage classes: %w", err)
	}

	cluster := BuildClusterState(nodes, classes, volumes, a.log)

	identity, err := FindSelf(cluster, a.cfg.NodeName)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Identity comes from the Node object; status comes from memory. The
	// in-memory copy is authoritative for the status half because a volume
	// staged a millisecond ago has not reached the informer cache yet, and
	// reading it back from there would un-export a device the kubelet is
	// waiting on.
	a.self.ID = identity.ID
	a.self.Cohort = identity.Cohort
	a.self.Zone = identity.Zone

	if err := a.assignFabricMinors(cluster); err != nil {
		return err
	}

	// Publish the local view of this node into the snapshot, so the derivation
	// sees the devices and minors that exist right now rather than the ones the
	// informer cache remembers.
	cluster = withSelf(cluster, a.self)

	if a.cfg.FabricEnabled() {
		a.reconcileFabric(cluster)
	}

	if err := a.render(); err != nil {
		return err
	}

	return a.publishStatus(ctx)
}

// assignFabricMinors gives every universe this node joins a local ublk minor
// for its fabric device, and takes back the minors of universes it has left.
//
// The minor is the device id in the config and therefore the device node
// number, so it must be stable for as long as the universe is joined: peers
// have namespaces pointing at /dev/ublkb<minor> and moving it means a
// disruptive namespace repoint.
func (a *Agent) assignFabricMinors(cluster racerctrl.ClusterState) error {
	joined := map[uint32]struct{}{}

	for _, universe := range cluster.Universes {
		if !universeJoinsNode(universe, a.self) {
			continue
		}

		joined[universe.ID] = struct{}{}

		if _, _, err := racerctrl.AssignFabricDeviceID(&a.self, universe.ID); err != nil {
			return fmt.Errorf("assign fabric minor for universe %d: %w", universe.ID, err)
		}
	}

	for _, export := range append([]racerctrl.FabricExport(nil), a.self.Fabric...) {
		if _, ok := joined[export.UniverseID]; !ok {
			racerctrl.ReleaseFabricDeviceID(&a.self, export.UniverseID)
		}
	}

	return nil
}

// reconcileFabric drives the NVMe-oF layer and records what it achieved.
//
// Fabric failures are logged rather than returned: a universe whose peer link
// could not be established renders without that peer, which racer accepts as a
// degraded group. Refusing to render at all would let one unreachable machine
// stall every node in the zone.
func (a *Agent) reconcileFabric(cluster racerctrl.ClusterState) {
	if a.fabric == nil {
		a.fabric = NewFabric(a.cfg, a.self.ID, nil)
	}

	plan := PlanFabric(a.fabric, cluster, a.self)

	state, err := a.fabric.Reconcile(plan)
	if err != nil {
		a.log.Error("fabric reconcile incomplete", "error", err)
	}

	a.attachments = state.Attachments

	// Carry the NQN and address the target actually got back into the fabric
	// annotation, since that is how peers learn what to attach.
	published := make(map[uint32]racerctrl.FabricExport, len(state.Exports))
	for _, export := range state.Exports {
		published[export.UniverseID] = export
	}

	for i, export := range a.self.Fabric {
		if got, ok := published[export.UniverseID]; ok {
			a.self.Fabric[i].NQN = got.NQN
			a.self.Fabric[i].Addr = got.Addr
		}
	}
}

// render derives, validates and installs the config.
func (a *Agent) render() error {
	candidate, err := racerctrl.Derive(racerctrl.Derivation{
		Cluster:     a.clusterSnapshot(),
		Self:        a.self,
		Attachments: a.attachments,
		Generation:  a.generation,
	})
	if err != nil {
		return fmt.Errorf("derive config: %w", err)
	}

	a.self.StoreBytes = candidate.GetNode().GetStore().GetSizeBytes()

	// Compare at the same generation. Bumping first and comparing after would
	// make every reconcile look like a change and burn a generation per event.
	if a.published != nil && proto.Equal(a.published, candidate) {
		return nil
	}

	candidate.Generation = a.generation + 1

	changed, err := racerctrl.Publish(a.cfg.ConfigPath(), a.published, candidate)
	if err != nil {
		return fmt.Errorf("publish config: %w", err)
	}

	if !changed {
		return nil
	}

	a.published = candidate
	a.generation = candidate.GetGeneration()

	a.log.Info("installed racer config",
		"generation", a.generation,
		"universes", len(candidate.GetUniverses()),
		"devices", len(candidate.GetDevices()),
		"storeBytes", candidate.GetNode().GetStore().GetSizeBytes())

	return nil
}

// publishStatus writes this node's status annotations if any of them changed.
func (a *Agent) publishStatus(ctx context.Context) error {
	desired := a.self.StatusAnnotations()

	node, err := a.nodeLister.Get(a.cfg.NodeName)
	if err != nil {
		return fmt.Errorf("get node %q: %w", a.cfg.NodeName, err)
	}

	changed := map[string]string{}

	for key, value := range desired {
		if node.Annotations[key] != value {
			changed[key] = value
		}
	}

	if len(changed) == 0 {
		return nil
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": changed},
	})
	if err != nil {
		return fmt.Errorf("build node annotation patch: %w", err)
	}

	_, err = a.client.CoreV1().Nodes().Patch(
		ctx, a.cfg.NodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch node %q annotations: %w", a.cfg.NodeName, err)
	}

	return nil
}

// scrapeLoop republishes racer's metrics as annotations.
//
// This is the only feedback channel in the system: racer has no status file and
// no API, so every sequenced operation the operator runs is gated on numbers
// that arrive here. A failed scrape leaves the previous values in place, which
// is the safe direction: stale nonzero values block a destructive sequence,
// they never unblock one.
func (a *Agent) scrapeLoop(ctx context.Context) {
	ticker := time.NewTicker(scrapeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples, err := a.scraper.Scrape(ctx)
		if err != nil {
			a.log.Warn("scrape of racer metrics failed", "url", a.cfg.MetricsURL, "error", err)
			continue
		}

		observation := Digest(samples)

		a.mu.Lock()
		before := racerctrl.FormatHealth(a.self.Health) + "|" + racerctrl.FormatLive(a.self.Live)
		a.self.Health = observation.Health
		a.self.Live = observation.Live
		after := racerctrl.FormatHealth(a.self.Health) + "|" + racerctrl.FormatLive(a.self.Live)
		a.mu.Unlock()

		if before != after {
			a.Trigger()
		}
	}
}

// Stage makes a volume available as a local block device.
//
// This is where a volume's placement is decided: the node picks a free local
// minor, records it, renders a config that exports the volume on that minor,
// and waits for the device node to appear. Placement is a node-local decision
// on purpose. The alternative, having the operator choose the minor, would put
// a round trip through the API server on the pod start path for a number that
// only this node can allocate correctly anyway.
func (a *Agent) Stage(ctx context.Context, volume string) (string, error) {
	a.mu.Lock()
	id, _, err := racerctrl.AssignDeviceID(&a.self, volume)
	a.mu.Unlock()

	if err != nil {
		return "", err
	}

	a.Trigger()

	path := racerctrl.BlockDevicePath(id)

	deadline, cancel := context.WithTimeout(ctx, a.cfg.StageTimeout)
	defer cancel()

	ticker := time.NewTicker(stagePollInterval)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}

		select {
		case <-deadline.Done():
			return "", fmt.Errorf(
				"timed out after %s waiting for %s: racer has not accepted a config exporting volume %q "+
					"(check racer_config_rejected_total and the racer container's log)",
				a.cfg.StageTimeout, path, volume)
		case <-ticker.C:
		}
	}
}

// Unstage stops exporting a volume and releases its minor.
//
// It does not wait: once the device is out of the rendered config racer stops
// exporting it, and the kubelet has already stopped using it or it would not be
// calling this.
func (a *Agent) Unstage(volume string) {
	a.mu.Lock()
	released := racerctrl.ReleaseDeviceID(&a.self, volume)
	a.mu.Unlock()

	if released {
		a.Trigger()
	}
}

// DevicePath reports the local device a volume is exported on, if it is.
func (a *Agent) DevicePath(volume string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, binding := range a.self.Devices {
		if binding.Volume == volume {
			return racerctrl.BlockDevicePath(binding.DeviceID), true
		}
	}

	return "", false
}

// NodeID reports this node's assigned racer id, or zero if the operator has not
// admitted it yet.
func (a *Agent) NodeID() uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.self.ID
}

// Zone reports this node's zone, or zero if it has not been assigned one.
func (a *Agent) Zone() uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.self.Zone
}

// clusterSnapshot re-reads the informer caches for the derivation. The caller
// holds the lock.
func (a *Agent) clusterSnapshot() racerctrl.ClusterState {
	nodes, _ := a.nodeLister.List(labelsEverything())     //nolint:errcheck
	volumes, _ := a.volumeLister.List(labelsEverything()) //nolint:errcheck
	classes, _ := a.classLister.List(labelsEverything())  //nolint:errcheck

	return withSelf(BuildClusterState(nodes, classes, volumes, a.log), a.self)
}

// withSelf replaces this node's entry in a snapshot with the agent's in-memory
// copy, and appends it if the informer cache has not caught up yet.
func withSelf(state racerctrl.ClusterState, self racerctrl.NodeState) racerctrl.ClusterState {
	for i := range state.Nodes {
		if state.Nodes[i].Name == self.Name {
			state.Nodes[i] = self

			return state
		}
	}

	if self.ID != 0 {
		state.Nodes = append(state.Nodes, self)
	}

	return state
}

// labelsEverything is the selector the listers take. Every object of these
// three kinds is relevant: a Node is a potential catalog member, a StorageClass
// is a potential universe, and a PersistentVolume is a potential extent, and
// none of them carry a label that would narrow that usefully.
func labelsEverything() labels.Selector {
	return labels.Everything()
}
