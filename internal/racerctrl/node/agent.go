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
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	// scrapeFailureThreshold is how many consecutive failed scrapes withdraw
	// this node's health entirely.
	//
	// The sequencers have no clock and no notion of a stale reading, so
	// freshness has to be expressed as presence or absence: counters that are
	// there, or a node that has said nothing. One failure is a restarting
	// sidecar and the last reading is still the best answer; several in a row
	// means nobody knows what the dataplane is doing, and the counters that
	// would be read as agreement have to go.
	scrapeFailureThreshold = 3

	// stagePollInterval is how often NodeStageVolume checks for the device.
	stagePollInterval = 250 * time.Millisecond

	// fabricRetry is how long an incomplete fabric reconcile waits before it
	// is attempted again. Long enough that a zone whose peers are genuinely
	// unreachable is not rewriting configfs continuously, short enough that a
	// node whose dataplane has just come back joins its universe promptly.
	fabricRetry = 5 * time.Second
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

	// memberLister watches the membership ConfigMaps of this node's own zone
	// and nothing else. It cannot be part of the factory above because the
	// selector needs the zone id, and the zone is only known once the operator
	// has stamped it on the Node object. Guarded by mu.
	memberLister corelisters.ConfigMapLister

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

	// adopted names the volumes whose bindings came off disk at startup rather
	// than from a NodeStageVolume in this process. They are pruned once, on the
	// first reconcile that sees a synced cache, against the volumes the cluster
	// actually has: a binding for a volume that has since been deleted would
	// otherwise fail every render forever. Bindings this process made itself
	// are never pruned, because a volume staged a moment ago may not have
	// reached the informer cache yet.
	adopted map[string]struct{}
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
	if err := a.adoptExistingState(); err != nil {
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

// adoptExistingState reads what an earlier run of this agent left behind: the
// config racer is already serving, and the device bindings that say which
// volume each of its minors carries.
//
// R1 requires the generation strictly increase for the life of a node, and a
// restarted agent that began again at one would have every subsequent config
// rejected by a racer that had not restarted with it. The bindings matter for a
// blunter reason: racer is a separate container in this pod and does not
// restart when the agent does, so its exports are still up and still in use.
// An agent that forgot them would render a config with no devices and take
// them away from running pods.
func (a *Agent) adoptExistingState() error {
	if err := os.MkdirAll(a.cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("create config directory %s: %w", a.cfg.ConfigDir, err)
	}

	a.adoptBindings()

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

	// The facts this generation carries are a property of the file, not of the
	// process that wrote it, so a restarted agent can state them again without
	// republishing anything. Leaving them out would make every gate treat this
	// node as one that has installed nothing until the next render happened to
	// change something.
	a.self.Applied = racerctrl.AppliedFrom(existing)

	a.log.Info("adopted existing racer config",
		"path", a.cfg.ConfigPath(),
		"generation", a.generation,
		"devices", len(a.self.Devices),
		"fabric", len(a.self.Fabric))

	return nil
}

// adoptBindings restores the volume-to-minor map from disk.
//
// A file that cannot be read is reported and ignored rather than fatal. The
// alternative, refusing to start, leaves the node with no agent at all: no
// status, no membership convergence and no way to recover, over a file the
// next successful stage rewrites. Losing the bindings costs the exports that
// were up, which is bad, but wedging the node costs everything.
func (a *Agent) adoptBindings() {
	stored, err := readBindings(a.cfg.BindingsPath())
	if err != nil {
		a.log.Error("ignoring unreadable device bindings; exports staged before this restart will be dropped",
			"path", a.cfg.BindingsPath(), "error", err)

		return
	}

	stored.apply(&a.self)

	a.adopted = make(map[string]struct{}, len(stored.Devices))
	for _, device := range stored.Devices {
		a.adopted[device.Volume] = struct{}{}
	}
}

// saveBindings records the current minors. The caller holds the lock.
//
// A failure here is logged rather than returned: the binding is already live in
// memory and the export it describes is what the pod is using, so refusing the
// operation would break a working path to protect a restart that may never
// happen.
func (a *Agent) saveBindings() {
	if err := writeBindings(a.cfg.BindingsPath(), a.self); err != nil {
		a.log.Error("failed to record device bindings; a restart would forget them",
			"path", a.cfg.BindingsPath(), "error", err)
	}
}

// pruneAdoptedBindings drops bindings restored from disk whose volume the
// cluster no longer has. The caller holds the lock.
//
// Without this a volume deleted while the agent was down would fail every
// render for the life of the pod, because the derivation refuses to build a
// device for a volume no storage class carries. Only adopted bindings are
// eligible: one this process made itself may be for a PersistentVolume the
// informer cache has not seen yet.
func (a *Agent) pruneAdoptedBindings(cluster racerctrl.ClusterState) {
	if len(a.adopted) == 0 {
		return
	}

	known := make(map[string]struct{})

	for i := range cluster.Universes {
		for j := range cluster.Universes[i].Volumes {
			known[cluster.Universes[i].Volumes[j].Name] = struct{}{}
		}
	}

	var dropped bool

	for volume := range a.adopted {
		if _, ok := known[volume]; ok {
			continue
		}

		if racerctrl.ReleaseDeviceID(&a.self, volume) {
			a.log.Warn("dropping a device binding for a volume the cluster no longer has",
				"volume", volume)

			dropped = true
		}
	}

	// One pass only. Every binding that survived it is now as trustworthy as
	// one this process staged itself.
	a.adopted = nil

	if dropped {
		a.saveBindings()
	}
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

	a.mu.Lock()
	defer a.mu.Unlock()

	// The zone has to be resolved before the memberships can be listed, since
	// the informer that holds them is selected on it.
	nodeStates := buildNodeStates(nodes, a.log)

	identity, err := FindSelf(racerctrl.ClusterState{Nodes: nodeStates}, a.cfg.NodeName)
	if err != nil {
		return err
	}

	memberships, err := a.memberships(ctx, identity.Zone)
	if err != nil {
		return err
	}

	cluster := racerctrl.ClusterState{
		Nodes:     nodeStates,
		Universes: buildUniverseStates(classes, volumes, memberships, a.log),
	}

	// Bindings restored from disk are checked against the cluster once, now
	// that there is a synced view to check them against.
	a.pruneAdoptedBindings(cluster)

	// Identity comes from the Node object; status comes from memory. The
	// in-memory copy is authoritative for the status half because a volume
	// staged a millisecond ago has not reached the informer cache yet, and
	// reading it back from there would un-export a device the kubelet is
	// waiting on.
	a.self.ID = identity.ID
	a.self.Cohort = identity.Cohort
	a.self.Zone = identity.Zone

	// Fabric identity and RDMA address are declared by an administrator on the
	// Node and never written back, so they are copied in every pass rather
	// than latched: recabling a node is an annotation edit, not a restart.
	a.self.FabricID = identity.FabricID
	a.self.RDMAAddr = identity.RDMAAddr

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
	changed := false

	for _, universe := range cluster.Universes {
		if !universeJoinsNode(universe, a.self) {
			continue
		}

		joined[universe.ID] = struct{}{}

		_, added, err := racerctrl.AssignFabricDeviceID(&a.self, universe.ID)
		if err != nil {
			return fmt.Errorf("assign fabric minor for universe %d: %w", universe.ID, err)
		}

		changed = changed || added
	}

	for _, export := range append([]racerctrl.FabricExport(nil), a.self.Fabric...) {
		if _, ok := joined[export.UniverseID]; !ok {
			changed = racerctrl.ReleaseFabricDeviceID(&a.self, export.UniverseID) || changed
		}
	}

	if changed {
		a.saveBindings()
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

	a.fabric.SetRDMAAddr(a.self.RDMAAddr)

	plan := PlanFabric(a.fabric, cluster, a.self)

	state, err := a.fabric.Reconcile(plan)
	if err != nil {
		a.log.Error("fabric reconcile incomplete", "error", err)

		// Ask for another pass. Most of what can go wrong here is a wait
		// rather than a fault: a peer that has not published its NQN yet, or
		// an export whose device the dataplane is still creating. Nothing in
		// the cluster changes when that resolves, so there is no watch event
		// to bring us back and the retry has to be scheduled.
		time.AfterFunc(fabricRetry, a.Trigger)
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
		Established: racerctrl.EstablishedUniverses(a.published),
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

	if len(candidate.GetUniverses()) == 0 {
		// This node is in no catalog and no draining set: it has handed
		// everything over and has nothing left to serve. Racer refuses a config
		// that names no universe, so there is nothing to install; the last one
		// stays in force, doing nothing, until the operator retires the
		// identity and the pod goes away. Status keeps being published, which
		// is what lets the operator see the counters that say it is idle.
		if a.published != nil {
			a.log.Info("this node joins no universe; leaving the installed config in force",
				"generation", a.generation)
		}

		return nil
	}

	// Every change the control plane makes is a step, so the generation always
	// advances by one. Membership moves one group slot at a time and a group
	// keeps a quorum across a generation, so there is no longer any change too
	// wide to be published as a transition.
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

	// What racer reports is which generation is in force, never what is in it.
	// Recording which facts this generation carried is what lets a sequencer
	// tell a node that has acted on a change from one that has not heard of it.
	a.self.Applied = racerctrl.AppliedFrom(candidate)

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
// that arrive here.
//
// A failed scrape leaves the previous values in place, because one missed
// reading is nearly always a restarting sidecar rather than a fact about the
// dataplane. But leaving them there forever is not safe in the other direction:
// a zero this node published before its metrics stopped being readable is a
// zero a destructive sequence will act on, and nothing else would ever
// contradict it. So after a few consecutive failures the health is withdrawn
// altogether, which every gate reads as no report rather than as agreement.
// There is no clock in this: the count of failed scrapes is the measure, so a
// node whose agent is not running publishes nothing new either way.
func (a *Agent) scrapeLoop(ctx context.Context) {
	ticker := time.NewTicker(scrapeInterval)
	defer ticker.Stop()

	failures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples, err := a.scraper.Scrape(ctx)
		if err != nil {
			a.log.Warn("scrape of racer metrics failed", "url", a.cfg.MetricsURL, "error", err)

			failures++
			if failures < scrapeFailureThreshold {
				continue
			}

			if a.forgetHealth() {
				a.log.Warn("withdrawing this node's health, its metrics have not been readable",
					"scrapes", failures)
				a.Trigger()
			}

			continue
		}

		failures = 0
		observation := Digest(samples)

		a.mu.Lock()
		before := a.healthDigest()
		a.self.Health = observation.Health
		a.self.Live = observation.Live
		after := a.healthDigest()
		a.mu.Unlock()

		if before != after {
			a.Trigger()
		}
	}
}

// forgetHealth withdraws the counters this node last reported, and says whether
// there was anything to withdraw. Zeroing Applied.Generation with them is what
// makes the withdrawal total: a gate reads that as a node whose agent has not
// installed anything, which is the one state no sequence proceeds from.
func (a *Agent) forgetHealth() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.self.Health == (racerctrl.Health{}) && len(a.self.Live) == 0 {
		return false
	}

	a.self.Health = racerctrl.Health{}
	a.self.Live = nil

	return true
}

// healthDigest is a cheap comparable rendering of what the last scrape saw,
// used only to decide whether the annotations are worth rewriting.
func (a *Agent) healthDigest() string {
	return racerctrl.FormatHealth(a.self.Health) + "|" +
		racerctrl.FormatLive(a.self.Live, a.self.Applied.Extents)
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

	id, added, err := racerctrl.AssignDeviceID(&a.self, volume)
	if err == nil && added {
		// Recorded before the config that exports it is rendered: a binding
		// racer is serving but the file does not name is exactly the state
		// this file exists to prevent.
		a.saveBindings()
	}

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
	if released {
		delete(a.adopted, volume)
		a.saveBindings()
	}

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

	var memberships []*corev1.ConfigMap
	if a.memberLister != nil {
		memberships, _ = a.memberLister.List(labelsEverything()) //nolint:errcheck
	}

	return withSelf(BuildClusterState(nodes, classes, volumes, memberships, a.log), a.self)
}

// memberships lists this node's zone's membership ConfigMaps, starting the
// informer that watches them on the first call.
//
// The watch is narrowed to one namespace and one zone label. A zone's
// membership is its entire catalog, up to a thousand node ids, and there may be
// sixty-four zones: handing every node all of them would put megabytes through
// every agent's watch to deliver the one list it can actually use. What a node
// needs of the other zones is their gateways, and those are small enough to
// stay on the StorageClass.
//
// The caller holds the lock.
func (a *Agent) memberships(ctx context.Context, zone uint32) ([]*corev1.ConfigMap, error) {
	if a.memberLister == nil {
		selector := labels.SelectorFromSet(labels.Set{
			racerctrl.MembershipZoneLabel: strconv.FormatUint(uint64(zone), 10),
		}).String()

		factory := informers.NewSharedInformerFactoryWithOptions(a.client, informerResync,
			informers.WithNamespace(a.cfg.Namespace),
			informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
				opts.LabelSelector = selector
			}))

		maps := factory.Core().V1().ConfigMaps()

		//nolint:errcheck // the handler registration only fails on a stopped informer
		_, _ = maps.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { a.Trigger() },
			UpdateFunc: func(any, any) { a.Trigger() },
			DeleteFunc: func(any) { a.Trigger() },
		})

		factory.Start(ctx.Done())

		syncCtx, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
		defer cancel()

		if !cache.WaitForCacheSync(syncCtx.Done(), maps.Informer().HasSynced) {
			return nil, fmt.Errorf("timed out waiting for the membership cache in namespace %s to sync; "+
				"check the racer-ctrl ClusterRole", a.cfg.Namespace)
		}

		a.memberLister = maps.Lister()
	}

	return a.memberLister.List(labelsEverything())
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
