//go:build e2e

// Package racere2e is the kind-based end-to-end test for racer and racer-ctrl.
//
// It is guarded by `//go:build e2e` so the default `go test ./...` skips it.
// Run via `make e2e-racer`, or directly with `go test -tags=e2e -timeout 60m
// ./e2e/racer/...`.
//
// # What this suite proves
//
// racer is three programs that only work together: the Rust dataplane that
// reads one config file and exports block devices, the racer-ctrl node agent
// that writes that file and manages the NVMe-oF fabric, and the operator
// component that decides identity, membership and extent placement for the
// whole cluster. Unit tests cover each of the three in isolation and a
// simulation harness covers the dataplane's consensus, but nothing before this
// ran all three against a real kernel and a real API server at once.
//
// The topology is two racer zones on one cluster - six nodes in one, three in
// the other - because almost everything interesting in racer is a function of
// zone shape. A six-node zone has two nodes per cohort and therefore catalog
// groups that share members; a three-node zone has exactly one trio; and a
// volume homed in one zone but read from the other is what the gateway path
// exists for.
//
// # Why one kernel is the hard part
//
// Every kind "node" is a container on one Linux kernel, and the two resources
// racer reaches for are kernel-global rather than per-container: ublk device
// minors and the nvmet configfs tree. Nine racer instances that each believe
// they own minor 1 collide on their first device, and nine agents that each
// create nvmet port 4420 end up publishing every subsystem on whichever node
// created the port first. Both were real defects rather than test artifacts -
// a leaked ublk device from a crashed instance wedges a production node the
// same way - and both are fixed in racer-ctrl rather than papered over here.
package racere2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	racercomponent "github.com/Azure/unbounded/internal/operator/components/racer"
	"github.com/Azure/unbounded/internal/racerctrl"
	"github.com/Azure/unbounded/internal/racerctrl/node"
)

const (
	// clusterName is the kind cluster. It doubles as the node name prefix,
	// which is how the harness maps a worker index onto a container.
	clusterName = "racer-e2e"

	// namespace is where the operator installs the node agent.
	namespace = "unbounded-system"

	// daemonSetName is what the operator calls the node agent workload.
	daemonSetName = "racer"

	// fieldManager is the suite's own server-side apply owner. It has to
	// differ from the operator's so that the two can hold disjoint fields of
	// the same DaemonSet without either reverting the other.
	fieldManager = "racer-e2e"

	// The two racer zones. Zone shape is the whole point of the topology, so
	// the sizes are named rather than derived: six gives two nodes per cohort
	// and catalog groups that overlap, three gives exactly one trio.
	alphaZoneName = "alpha"
	betaZoneName  = "beta"
	alphaWorkers  = 6
	betaWorkers   = 3
	workerCount   = alphaWorkers + betaWorkers

	// cpusPerWorker bounds each node container's cpuset.
	//
	// racer sizes its worker pool from sched_getaffinity folded over the SMT
	// sibling lists, so an unconstrained instance on this host would spawn one
	// worker per physical core. Nine of those is several hundred threads
	// fighting over a 24-core machine, and the io_uring rings they carry are
	// not free. Four CPUs per node is enough to exercise the multi-worker
	// paths while leaving the host responsive.
	cpusPerWorker = 4

	// imageRegistry and imageTag are what the operator composes container
	// references from. They never resolve against a registry: the images are
	// built locally and side-loaded into the cluster, and the DaemonSet pulls
	// IfNotPresent.
	imageRegistry = "racer.e2e.local"
	imageTag      = "e2e"

	// siteName is the Site the operator component is reconciled against. It is
	// synthesised rather than read from the API server: Component.Reconcile
	// takes Sites by value, so the suite needs no Site CRD.
	siteName = "racer-e2e-site"

	// className is the universe this suite works in, and catalogSize is the
	// number of groups its zones are cut into.
	//
	// The class is created here rather than left to the operator's default so
	// that the catalog can be small. Catalog size is the universe's own policy
	// knob, frozen for the life of the zone, and the default of 2520 is aimed
	// at a real fleet: it spreads a zone's address space finely enough that no
	// single trio is a hot spot. Anti-entropy and extent hand-over walk that
	// catalog one group per core per second, so a 2520-group zone on two-core
	// nodes takes twenty minutes to visit every group once, and a migration is
	// several of those passes. Twenty-four groups exercise exactly the same
	// code - groups still overlap in the six-node zone, and a group is still a
	// trio spanning three cohorts - in seconds rather than hours.
	//
	// The size has to divide by the per-cohort membership of every zone the
	// universe reaches: two in the six-node zone, one in the three-node zone,
	// and one again after the six-node zone is shrunk.
	className   = "racer-e2e"
	catalogSize = 24

	// minUblksMax is what racer-ctrl's preflight demands, because a node's
	// export budget is MaxExports and every one of them is a ublk device. The
	// kernel default is 64.
	minUblksMax = 256
)

// harness owns the cluster and everything derived from it.
type harness struct {
	t          *testing.T
	repoRoot   string
	artifacts  string
	storeRoot  string
	kubeconfig string

	cfg    *rest.Config
	cli    client.Client
	kube   kubernetes.Interface
	scheme *runtime.Scheme

	// workers are the kind worker node names in index order, so workers[0..5]
	// are the alpha zone and workers[6..8] are the beta zone.
	workers []string
}

// zoneNameFor is the racer zone a worker index belongs to. The operator honours
// the zone-name annotation absolutely, so this is what pins the 6/3 split
// rather than leaving it to automatic placement.
func zoneNameFor(index int) string {
	if index < alphaWorkers {
		return alphaZoneName
	}

	return betaZoneName
}

// workerName is kind's naming scheme: the first worker has no suffix.
func workerName(index int) string {
	if index == 0 {
		return clusterName + "-worker"
	}

	return fmt.Sprintf("%s-worker%d", clusterName, index+1)
}

// newHarness prepares the host, boots the cluster, and returns a harness whose
// clients are ready. Every failure before the cluster exists is a skip rather
// than a failure, because a machine without ublk or without docker cannot run
// this suite at all and saying so is more useful than a red test.
func newHarness(t *testing.T) *harness {
	t.Helper()

	root := repoRoot(t)

	h := &harness{
		t:         t,
		repoRoot:  root,
		artifacts: mkdir(t, filepath.Join(root, "tmp", "racer-e2e", "artifacts")),
		storeRoot: mkdir(t, filepath.Join(root, "tmp", "racer-e2e", "stores")),
	}

	h.requireTools()
	h.requireKernel()
	h.buildImages()
	h.bootCluster()
	h.constrainCPUs()
	h.loadImages()
	h.connect()
	h.ensureNamespace()
	h.resetWorkload()
	h.enrollNodes()

	return h
}

// resetWorkload puts the cluster back to the state a fresh one would be in.
//
// The suite reuses a kind cluster between runs because creating one costs
// minutes, but two pieces of state outlive a run and would poison the next
// one. The DaemonSet is applied by the operator loop and would otherwise keep
// running against whatever images and environment the previous run left it
// with; and each worker's store directory is a host directory, so the bindings
// file that records which ublk minor this node claimed survives the pod that
// wrote it. A stale binding is not merely untidy - it is authoritative, and an
// agent that reads one never reconsiders the minor inside it.
//
// The order matters: the pods have to be gone before the store directories are
// touched, or a live agent rewrites the file that was just removed.
func (h *harness) resetWorkload() {
	h.t.Helper()

	ctx := context.Background()

	h.resetVolumes(ctx)

	set := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: daemonSetName, Namespace: namespace}}
	if err := h.cli.Delete(ctx, set); err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("delete daemonset: %v", err)
	}

	deadline := time.Now().Add(2 * time.Minute)

	for {
		pods := &corev1.PodList{}
		if err := h.cli.List(ctx, pods, client.InNamespace(namespace)); err != nil {
			h.t.Fatalf("list pods: %v", err)
		}

		live := 0

		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, daemonSetName+"-") {
				live++
			}
		}

		if live == 0 {
			break
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("%d racer pods still present after deleting the daemonset", live)
		}

		time.Sleep(time.Second)
	}

	for _, name := range h.workers {
		dir := filepath.Join(h.storeRoot, name)
		if _, err := os.Stat(dir); err != nil {
			continue
		}

		// The store and the bindings file are written by a privileged
		// container as root, so the test user cannot remove them itself.
		// -mindepth 1 keeps the directory: it is bind-mounted into a node
		// container, and replacing it would leave that container looking at
		// an inode nothing else can reach.
		if err := h.sudo(ctx, "find", dir, "-mindepth", "1", "-delete"); err != nil {
			h.t.Fatalf("clear %s: %v", dir, err)
		}
	}

	h.resetFabric(ctx)
	h.resetUniverses(ctx)
	h.ensureClass(ctx)
}

// resetUniverses drops the universes an earlier run left behind.
//
// A universe's catalog size is frozen when its zones are seeded, so a class
// left over from a run that used a different one would quietly keep it. The
// memberships go with it: they name a universe id that will not be minted
// again, and nothing collects them once the class that owned them is gone.
//
// The node identities are deliberately left alone. They live in annotations on
// the Node objects and in the operator's allocation cursors, and resetting one
// without the other would hand out an id a node still holds.
func (h *harness) resetUniverses(ctx context.Context) {
	h.t.Helper()

	classes := &storagev1.StorageClassList{}
	if err := h.cli.List(ctx, classes); err != nil {
		h.t.Fatalf("list storage classes: %v", err)
	}

	for i := range classes.Items {
		class := &classes.Items[i]
		if class.Provisioner != racerctrl.DriverName {
			continue
		}

		if err := h.cli.Delete(ctx, class); err != nil && !apierrors.IsNotFound(err) {
			h.t.Fatalf("delete storage class %s: %v", class.Name, err)
		}
	}

	maps := &corev1.ConfigMapList{}

	err := h.cli.List(ctx, maps, client.InNamespace(namespace),
		client.HasLabels{racerctrl.MembershipUniverseLabel})
	if err != nil {
		h.t.Fatalf("list membership configmaps: %v", err)
	}

	for i := range maps.Items {
		if err := h.cli.Delete(ctx, &maps.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			h.t.Fatalf("delete membership %s: %v", maps.Items[i].Name, err)
		}
	}
}

// ensureClass creates the universe this suite works in.
//
// The operator creates a default class when it finds none, so this only has to
// win the race, which it does by running before the operator loop starts. What
// it is really for is the catalog size: the operator stamps its default only
// onto a class that carries none, so a class that names its own keeps it.
func (h *harness) ensureClass(ctx context.Context) {
	h.t.Helper()

	binding := storagev1.VolumeBindingWaitForFirstConsumer
	reclaim := corev1.PersistentVolumeReclaimDelete
	expansion := false

	class := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
			Annotations: map[string]string{
				racerctrl.CatalogSizeAnnotation: strconv.Itoa(catalogSize),
			},
		},
		Provisioner:          racerctrl.DriverName,
		VolumeBindingMode:    &binding,
		ReclaimPolicy:        &reclaim,
		AllowVolumeExpansion: &expansion,
	}

	if err := h.cli.Create(ctx, class); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create storage class %s: %v", className, err)
	}
}

// resetFabric strips the nvmet configuration an earlier run left in the kernel.
//
// nvmet lives in a single configfs tree that every node container shares with
// the host, and it is not owned by any pod, so a run that ended abruptly leaves
// nine subsystems behind. Each of them holds a namespace over a ublk device
// that no longer exists, and, worse, holds that device open, which is what
// stops the driver from ever returning the minor. The agent now disables such a
// namespace on its own, but the next run should not have to wait out a
// crash-loop to get there, and a subsystem belonging to a node id this run will
// not reuse would simply linger.
func (h *harness) resetFabric(ctx context.Context) {
	h.t.Helper()

	// Ordering is forced by the kernel: the initiators have to go first, or a
	// controller still on its reconnect timer rebuilds its link to a subsystem
	// this is trying to remove; a subsystem cannot be removed while a port
	// links it or a host is allowed onto it; and a port cannot be removed while
	// it links a subsystem.
	script := strings.Join([]string{
		"set -e",
		"root=/sys/kernel/config/nvmet",
		"for ctrl in /sys/class/nvme/nvme*; do",
		"  [ -e \"$ctrl\"/hostnqn ] || continue",
		"  case \"$(cat \"$ctrl\"/hostnqn)\" in",
		"    " + node.DefaultNQNPrefix + "*) echo 1 > \"$ctrl\"/delete_controller || true ;;",
		"  esac",
		"done",
		"[ -d $root ] || exit 0",
		"for sub in $root/subsystems/" + node.DefaultNQNPrefix + "*; do",
		"  [ -d \"$sub\" ] || continue",
		"  rm -f $root/ports/*/subsystems/\"$(basename \"$sub\")\"",
		"  rm -f \"$sub\"/allowed_hosts/*",
		"  rmdir \"$sub\"/namespaces/* 2>/dev/null || true",
		"  rmdir \"$sub\" 2>/dev/null || true",
		"done",
		"rmdir $root/hosts/" + node.DefaultNQNPrefix + "* 2>/dev/null || true",
		"rmdir $root/ports/* 2>/dev/null || true",
		"exit 0",
	}, "\n")

	if err := h.sudo(ctx, "sh", "-c", script); err != nil {
		h.t.Fatalf("clear nvmet: %v", err)
	}
}

// resetVolumes removes what an earlier run of the suite left behind.
//
// The suite is meant to be re-runnable against a kept cluster, and a run that
// was interrupted leaves consumer pods holding devices, claims bound to them,
// and volumes carrying racer's finalizer. Nothing will release that finalizer
// once the operator loop of the old run is gone, and the stores are about to
// be wiped anyway, so it is stripped rather than drained.
func (h *harness) resetVolumes(ctx context.Context) {
	h.t.Helper()

	pods := &corev1.PodList{}
	if err := h.cli.List(ctx, pods, client.InNamespace(claimNS)); err != nil {
		h.t.Fatalf("list consumer pods: %v", err)
	}

	grace := int64(0)

	for i := range pods.Items {
		pod := &pods.Items[i]
		if !strings.HasPrefix(pod.Name, "racer-consumer-") {
			continue
		}

		if err := h.cli.Delete(ctx, pod, client.GracePeriodSeconds(grace)); err != nil &&
			!apierrors.IsNotFound(err) {
			h.t.Fatalf("delete pod %s: %v", pod.Name, err)
		}
	}

	claims := &corev1.PersistentVolumeClaimList{}
	if err := h.cli.List(ctx, claims, client.InNamespace(claimNS)); err != nil {
		h.t.Fatalf("list claims: %v", err)
	}

	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName == "" {
			continue
		}

		if err := h.cli.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
			h.t.Fatalf("delete claim %s: %v", claim.Name, err)
		}
	}

	volumes := &corev1.PersistentVolumeList{}
	if err := h.cli.List(ctx, volumes); err != nil {
		h.t.Fatalf("list volumes: %v", err)
	}

	for i := range volumes.Items {
		volume := &volumes.Items[i]
		if volume.Spec.CSI == nil || volume.Spec.CSI.Driver != racerctrl.DriverName {
			continue
		}

		if err := h.cli.Delete(ctx, volume); err != nil && !apierrors.IsNotFound(err) {
			h.t.Fatalf("delete volume %s: %v", volume.Name, err)
		}

		patch := []byte(`{"metadata":{"finalizers":null}}`)
		if err := h.cli.Patch(ctx, volume, client.RawPatch(types.MergePatchType, patch)); err != nil &&
			!apierrors.IsNotFound(err) {
			h.t.Fatalf("strip finalizers from %s: %v", volume.Name, err)
		}
	}
}

// shareKernel gives every agent a private window of ublk minors.
//
// Minors are global to the kernel, and nine racer instances on one kernel is
// not the arrangement racer-ctrl is built for: in production one node is one
// kernel and the default floor of 1 is right. RACER_DEVICE_ID_BASE=auto asks
// the agent to derive its window from the node id the operator gave it, which
// is unique cluster-wide, so the nine agents never contend.
//
// The patch is a server-side apply under this suite's own field manager rather
// than an edit of the rendered manifest, because the operator re-applies the
// DaemonSet every couple of seconds and would revert anything it owns. It does
// not own this entry: both containers and env are keyed lists, so an apply
// naming only one container and one variable adds that variable and leaves the
// rest of the object to whoever wrote it.
func (h *harness) shareKernel(ctx context.Context) {
	h.t.Helper()

	deadline := time.Now().Add(3 * time.Minute)

	for {
		set := &appsv1.DaemonSet{}

		err := h.cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: daemonSetName}, set)
		if err == nil {
			break
		}

		if !apierrors.IsNotFound(err) {
			h.t.Fatalf("get daemonset: %v", err)
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("daemonset %s/%s never appeared", namespace, daemonSetName)
		}

		time.Sleep(time.Second)
	}

	patch := fmt.Sprintf(`{
"apiVersion":"apps/v1","kind":"DaemonSet",
"metadata":{"name":%q,"namespace":%q},
"spec":{"template":{"spec":{"containers":[
{"name":"racer-ctrl","env":[{"name":%q,"value":%q}]}]}}}}`,
		daemonSetName, namespace, node.EnvDeviceIDBase, node.DeviceIDBaseAuto)

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON([]byte(patch)); err != nil {
		h.t.Fatalf("build patch: %v", err)
	}

	err := h.cli.Patch(ctx, obj, client.RawPatch(types.ApplyPatchType, []byte(patch)),
		client.FieldOwner(fieldManager), client.ForceOwnership)
	if err != nil {
		h.t.Fatalf("share the kernel: %v", err)
	}
}

// growStores restarts the dataplane so it picks up a larger store.
//
// racer sizes its allocator once, at startup: it formats or grows the store
// file to the size the configuration asks for and never revisits that while it
// runs. A node that has just been handed a new extent therefore reports
// unbacked pages, and writes to that extent fail with ENOSPC until the process
// comes back. That is deliberate rather than a defect - the metric's own help
// text says it stays nonzero until a restart grows the store, and the operator
// only reports the condition, because deciding when to bounce a storage fleet
// belongs to whoever runs it. This suite is that administrator.
//
// The restart has to happen before anything stages a volume. A racer that exits
// takes its ublk devices with it, so a pod already holding one would be left
// with a device node that answers nothing.
func (h *harness) growStores(t *testing.T, ctx context.Context) {
	t.Helper()

	var hungry []string

	h.waitFor(t, ctx, 3*time.Minute, "the dataplane to say whether its store is large enough", func() error {
		states, err := h.nodeStates(ctx)
		if err != nil {
			return err
		}

		hungry = nil

		for _, worker := range h.workers {
			state, ok := states[worker]
			if !ok {
				return fmt.Errorf("%s has published no state", worker)
			}

			if gate := racerctrl.ConfigLoaded(state); !gate.OK {
				return fmt.Errorf("%s: %s", worker, gate.Reason)
			}

			if state.Health.UnbackedPages > 0 {
				hungry = append(hungry, worker)
			}
		}

		return nil
	})

	if len(hungry) == 0 {
		t.Logf("every store already backs the pages it was given")

		return
	}

	t.Logf("restarting the dataplane so %d stores grow: %s", len(hungry), strings.Join(hungry, " "))
	h.restartDataplane(t, ctx, hungry)

	h.waitFor(t, ctx, 5*time.Minute, "every store to back the pages it was given", func() error {
		states, err := h.nodeStates(ctx)
		if err != nil {
			return err
		}

		for _, worker := range h.workers {
			state, ok := states[worker]
			if !ok {
				return fmt.Errorf("%s has published no state", worker)
			}

			if gate := racerctrl.ConfigLoaded(state); !gate.OK {
				return fmt.Errorf("%s: %s", worker, gate.Reason)
			}

			if state.Health.UnbackedPages > 0 {
				return fmt.Errorf("%s still reports %d unbacked pages", worker, state.Health.UnbackedPages)
			}
		}

		return nil
	})
}

// restartDataplane deletes the racer pods on the named nodes and waits for the
// replacements.
//
// This is what an administrator does after the operator reports that stores
// need to grow, and it doubles as the restart-tolerance scenario: everything a
// node knows has to come back from its store file and the API server, because
// the config directory is an emptyDir that goes away with the pod.
//
// Only the named nodes are restarted. A racer that exits takes its ublk devices
// with it, so bouncing a node that is exporting a volume to a running pod would
// leave that pod holding a device node that answers nothing. Growing one zone
// while the other keeps serving is also what an administrator would do.
func (h *harness) restartDataplane(t *testing.T, ctx context.Context, nodes []string) {
	t.Helper()

	wanted := make(map[string]bool, len(nodes))
	for _, name := range nodes {
		wanted[name] = true
	}

	pods := &corev1.PodList{}
	if err := h.cli.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list racer pods: %v", err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if !strings.HasPrefix(pod.Name, daemonSetName+"-") || !wanted[pod.Spec.NodeName] {
			continue
		}

		if err := h.cli.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete pod %s: %v", pod.Name, err)
		}
	}

	h.waitFor(t, ctx, 5*time.Minute, "the dataplane to come back", func() error {
		set := &appsv1.DaemonSet{}

		err := h.cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: daemonSetName}, set)
		if err != nil {
			return err
		}

		if set.Status.NumberReady != workerCount || set.Status.NumberUnavailable != 0 {
			return fmt.Errorf("%d of %d ready, %d unavailable",
				set.Status.NumberReady, workerCount, set.Status.NumberUnavailable)
		}

		return nil
	})
}

// requireTools skips unless the CLIs the harness drives are usable.
func (h *harness) requireTools() {
	h.t.Helper()

	for _, tool := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			h.t.Skipf("%s not on PATH", tool)
		}
	}

	if err := h.run(context.Background(), "docker", "info"); err != nil {
		h.t.Skipf("docker is not usable: %v", err)
	}
}

// requireKernel makes the host able to carry a whole racer zone.
//
// Two module parameters decide whether the suite can run at all. ublks_max
// bounds how many ublk devices exist on the machine and defaults to 64, which
// is below the export budget of a single node let alone nine; and nvmet's
// transport modules have to be loaded before any agent can create a subsystem,
// because configfs only grows the nvmet tree once the target core is there.
func (h *harness) requireKernel() {
	h.t.Helper()

	ctx := context.Background()

	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		if err := h.sudo(ctx, "modprobe", "ublk_drv", "ublks_max="+strconv.Itoa(minUblksMax*2)); err != nil {
			h.t.Skipf("ublk_drv is not available: %v", err)
		}
	}

	if max := readUint(h.t, "/sys/module/ublk_drv/parameters/ublks_max"); max < minUblksMax {
		// The parameter is read-only once loaded, so raising it means
		// reloading the driver. Refuse rather than reload if anything is
		// already using it: unloading ublk_drv under a live device would take
		// out whatever owns it.
		if devices := ublkDevices(); len(devices) > 0 {
			h.t.Skipf("ublks_max is %d (need %d) and %d ublk devices are in use, so the driver cannot be reloaded",
				max, minUblksMax, len(devices))
		}

		if err := h.sudo(ctx, "modprobe", "-r", "ublk_drv"); err != nil {
			h.t.Skipf("cannot reload ublk_drv to raise ublks_max: %v", err)
		}

		if err := h.sudo(ctx, "modprobe", "ublk_drv", "ublks_max="+strconv.Itoa(minUblksMax*2)); err != nil {
			h.t.Fatalf("reload ublk_drv: %v", err)
		}
	}

	for _, module := range []string{"nvmet", "nvmet-tcp", "nvme-tcp"} {
		if err := h.sudo(ctx, "modprobe", module); err != nil {
			h.t.Skipf("cannot load %s, which the fabric needs: %v", module, err)
		}
	}

	if _, err := os.Stat("/sys/kernel/config/nvmet"); err != nil {
		h.t.Skipf("nvmet configfs is not mounted: %v", err)
	}
}

// buildImages builds the two images the operator will name. E2E_SKIP_BUILD=1
// reuses whatever is already tagged, which is what iterating on the test itself
// wants.
func (h *harness) buildImages() {
	h.t.Helper()

	if os.Getenv("E2E_SKIP_BUILD") == "1" {
		h.t.Log("E2E_SKIP_BUILD=1; reusing the existing images")

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	for _, target := range []string{"image-racer-local", "image-racer-ctrl-local"} {
		cmd := exec.CommandContext(ctx, "make", "-C", h.repoRoot, target,
			"RACER_IMAGE="+imageRef("racer"),
			"RACER_CTRL_IMAGE="+imageRef("racer-ctrl"),
		)
		cmd.Env = append(os.Environ(), "CONTAINER_ENGINE=docker")

		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Fatalf("make %s: %v\n%s", target, err, tail(out, 60))
		}
	}
}

// imageRef is the reference the operator will compose for a repository, which
// is also what the build has to produce.
func imageRef(repo string) string {
	return fmt.Sprintf("%s/%s:%s", imageRegistry, repo, imageTag)
}

// loadImages side-loads the two locally built images.
//
// It goes through a `docker save` archive rather than `kind load docker-image`,
// because the direct path asks containerd to import with --all-platforms
// --digests and one node at random then fails on a blob the daemon never
// pulled. The registrar is deliberately not side-loaded at all: its local copy
// is a manifest list carrying attestation manifests, both kind load paths ask
// for blobs that are not in the local store, and the node containers can reach
// registry.k8s.io perfectly well on their own.
func (h *harness) loadImages() {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	archive := filepath.Join(h.artifacts, "images.tar")
	images := []string{imageRef("racer"), imageRef("racer-ctrl")}

	if err := h.run(ctx, "docker", append([]string{"save", "-o", archive}, images...)...); err != nil {
		h.t.Fatalf("save images: %v", err)
	}

	defer func() { _ = os.Remove(archive) }()

	if err := h.run(ctx, "kind", "load", "image-archive", archive, "--name", clusterName); err != nil {
		h.t.Fatalf("kind load image-archive: %v", err)
	}
}

// kindConfig is the cluster definition.
//
// Three things make it unusual. The host's /dev is bound into every worker
// because ublk device nodes are created by devtmpfs on the host and a
// container's private /dev would never see them. The host's configfs is bound
// in because nvmet's control plane is configfs and there is only one of it per
// kernel. And each worker's store lives on a host directory rather than the
// node's overlay, because racer-ctrl's preflight insists the store filesystem
// honours O_DIRECT and RWF_DSYNC, which overlayfs does not reliably do.
func (h *harness) kindConfig() string {
	var b strings.Builder

	b.WriteString("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nname: " + clusterName + "\nnodes:\n")
	b.WriteString("- role: control-plane\n")

	for i := range workerCount {
		store := mkdir(h.t, filepath.Join(h.storeRoot, workerName(i)))

		b.WriteString("- role: worker\n")
		b.WriteString("  extraMounts:\n")
		b.WriteString("  - hostPath: /dev\n    containerPath: /dev\n")
		b.WriteString("  - hostPath: /sys/kernel/config\n    containerPath: /sys/kernel/config\n")
		b.WriteString("  - hostPath: " + store + "\n    containerPath: /var/lib/racer\n")
	}

	return b.String()
}

// bootCluster creates the cluster, reusing one that is already up so a failed
// run can be re-entered without paying for a fresh boot.
func (h *harness) bootCluster() {
	h.t.Helper()

	h.kubeconfig = filepath.Join(h.artifacts, "kubeconfig")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	for i := range workerCount {
		h.workers = append(h.workers, workerName(i))
	}

	if h.clusterExists(ctx) {
		h.t.Logf("reusing the existing kind cluster %q", clusterName)

		if err := h.run(ctx, "kind", "export", "kubeconfig", "--name", clusterName, "--kubeconfig", h.kubeconfig); err != nil {
			h.t.Fatalf("export kubeconfig: %v", err)
		}

		h.registerTeardown()

		return
	}

	configPath := filepath.Join(h.artifacts, "kind-config.yaml")
	if err := os.WriteFile(configPath, []byte(h.kindConfig()), 0o600); err != nil {
		h.t.Fatalf("write kind config: %v", err)
	}

	if err := h.run(ctx, "kind", "create", "cluster",
		"--name", clusterName,
		"--config", configPath,
		"--kubeconfig", h.kubeconfig,
		"--wait", "5m",
	); err != nil {
		h.t.Fatalf("kind create cluster: %v", err)
	}

	h.registerTeardown()
}

func (h *harness) registerTeardown() {
	h.t.Cleanup(func() {
		if os.Getenv("E2E_KEEP") == "1" {
			h.t.Logf("E2E_KEEP=1; leaving kind cluster %q up", clusterName)

			return
		}

		h.collectArtifacts()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		_ = h.run(ctx, "kind", "delete", "cluster", "--name", clusterName)
	})
}

// clusterExists reports whether a cluster of this name is up and usable.
//
// kind lists a cluster whenever its node containers exist, running or not, and
// a half-deleted cluster from an earlier run would otherwise be adopted and
// then fail on the first API call. Requiring the control plane to be running
// makes the reuse path mean what it says.
func (h *harness) clusterExists(ctx context.Context) bool {
	out, err := h.output(ctx, "kind", "get", "clusters")
	if err != nil {
		return false
	}

	found := false

	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == clusterName {
			found = true
		}
	}

	if !found {
		return false
	}

	state, err := h.output(ctx, "docker", "inspect", "-f", "{{.State.Running}}", clusterName+"-control-plane")
	if err != nil || strings.TrimSpace(state) != "true" {
		h.t.Logf("kind lists cluster %q but its control plane is not running; recreating", clusterName)

		_ = h.run(ctx, "kind", "delete", "cluster", "--name", clusterName)

		return false
	}

	return true
}

// constrainCPUs narrows each worker container's cpuset so the nine racer
// instances between them do not try to own the machine.
func (h *harness) constrainCPUs() {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	available := onlineCPUs(h.t)
	if len(available) < workerCount*cpusPerWorker {
		h.t.Logf("only %d CPUs online; leaving worker cpusets alone", len(available))

		return
	}

	for i, worker := range h.workers {
		slice := available[i*cpusPerWorker : (i+1)*cpusPerWorker]
		set := make([]string, 0, len(slice))

		for _, cpu := range slice {
			set = append(set, strconv.Itoa(cpu))
		}

		if err := h.run(ctx, "docker", "update", "--cpuset-cpus", strings.Join(set, ","), worker); err != nil {
			h.t.Fatalf("constrain %s to CPUs %v: %v", worker, set, err)
		}
	}
}

// connect builds the typed and controller-runtime clients.
func (h *harness) connect() {
	h.t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", h.kubeconfig)
	if err != nil {
		h.t.Fatalf("build rest config: %v", err)
	}

	// The suite drives a controller in-process and polls hard; the client-side
	// defaults would throttle it into uselessness.
	cfg.QPS = 200
	cfg.Burst = 400

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
		storagev1.AddToScheme,
		unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			h.t.Fatalf("add to scheme: %v", err)
		}
	}

	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		h.t.Fatalf("build controller-runtime client: %v", err)
	}

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		h.t.Fatalf("build typed client: %v", err)
	}

	h.cfg, h.cli, h.kube, h.scheme = cfg, cli, kube, scheme
}

func (h *harness) ensureNamespace() {
	h.t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := h.cli.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create namespace %s: %v", namespace, err)
	}
}

// enrollNodes opts every worker into racer and pins which racer zone it lands
// in. The zone-name annotation is an operator input rather than a derived
// value, and placement honours it absolutely, so this is what turns nine
// interchangeable kind workers into a six-node zone and a three-node zone.
func (h *harness) enrollNodes() {
	h.t.Helper()

	ctx := context.Background()

	for i, name := range h.workers {
		patch := fmt.Sprintf(
			`{"metadata":{"labels":{%q:"true"},"annotations":{%q:%q}}}`,
			racercomponent.EnrollmentLabel,
			racerctrl.NodeZoneNameAnnotation, zoneNameFor(i),
		)

		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := h.cli.Patch(ctx, node, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			h.t.Fatalf("enrol %s: %v", name, err)
		}
	}
}

// site is the synthesised Site the component is reconciled against.
func site() unboundedv1alpha3.Site {
	enabled := true

	return unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: siteName},
		Spec: unboundedv1alpha3.SiteSpec{
			Components: unboundedv1alpha3.SiteComponents{
				Racer: &unboundedv1alpha3.RacerComponentSpec{
					SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
				},
			},
		},
	}
}

// env is the component environment the operator would build.
func (h *harness) env() *component.Env {
	return &component.Env{
		Client:    h.cli,
		Scheme:    h.scheme,
		Namespace: namespace,
		Config: component.Config{
			ImageRegistry: imageRegistry,
			ImageTag:      imageTag,
		},
	}
}

// reconcile runs one pass of the real operator component and returns its
// result. Driving it by hand rather than starting a manager is deliberate: the
// suite wants to say "advance the control plane once" at points of its own
// choosing, and to see the sequencing gate that blocked.
func (h *harness) reconcile(ctx context.Context) component.Result {
	h.t.Helper()

	sites := []unboundedv1alpha3.Site{site()}

	return racercomponent.Component{}.Reconcile(ctx, h.env(), sites)
}

// runOperator keeps reconciling in the background until the context is
// cancelled, which is what the component would experience under a manager. The
// returned function stops it and waits.
func (h *harness) runOperator(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			result := h.reconcile(ctx)
			if result.Err != nil && ctx.Err() == nil {
				h.t.Logf("operator pass failed: %v", result.Err)
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// ---------------------------------------------------------------------------
// process helpers
// ---------------------------------------------------------------------------

func (h *harness) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, tail(out, 40))
	}

	return nil
}

func (h *harness) output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	var stdout bytes.Buffer

	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return stdout.String(), nil
}

// sudo runs a privileged command, which the kernel preparation needs and
// nothing else does.
func (h *harness) sudo(ctx context.Context, args ...string) error {
	return h.run(ctx, "sudo", append([]string{"-n"}, args...)...)
}

// ---------------------------------------------------------------------------
// small utilities
// ---------------------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}

		dir = parent
	}
}

func mkdir(t *testing.T, path string) string {
	t.Helper()

	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}

	return path
}

func readUint(t *testing.T, path string) uint64 {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	fields := strings.FieldsFunc(strings.TrimSpace(string(raw)), func(r rune) bool {
		return r == ',' || r == '-'
	})
	if len(fields) == 0 {
		return 0
	}

	value, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	if err != nil {
		return 0
	}

	return value
}

// onlineCPUs expands /sys/devices/system/cpu/online into individual ids.
func onlineCPUs(t *testing.T) []int {
	t.Helper()

	raw, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil
	}

	var cpus []int

	for _, part := range strings.Split(strings.TrimSpace(string(raw)), ",") {
		bounds := strings.SplitN(part, "-", 2)

		low, err := strconv.Atoi(bounds[0])
		if err != nil {
			continue
		}

		high := low

		if len(bounds) == 2 {
			if parsed, err := strconv.Atoi(bounds[1]); err == nil {
				high = parsed
			}
		}

		for cpu := low; cpu <= high; cpu++ {
			cpus = append(cpus, cpu)
		}
	}

	return cpus
}

// ublkDevices lists the ublk character devices the kernel currently holds.
func ublkDevices() []string {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil
	}

	var devices []string

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "ublkc") {
			devices = append(devices, entry.Name())
		}
	}

	return devices
}

func tail(out []byte, lines int) string {
	split := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(split) > lines {
		split = split[len(split)-lines:]
	}

	return strings.Join(split, "\n")
}

// ---------------------------------------------------------------------------
// diagnostics
// ---------------------------------------------------------------------------

// collectArtifacts dumps everything worth reading after a failure into the
// artifacts directory. It runs before the cluster is deleted and never fails
// the test: a diagnostic that can break the teardown is worse than no
// diagnostic.
func (h *harness) collectArtifacts() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	h.dump(ctx, "nodes.yaml", "kubectl", "get", "nodes", "-o", "yaml")
	h.dump(ctx, "pods.txt", "kubectl", "get", "pods", "-n", namespace, "-o", "wide")
	h.dump(ctx, "daemonset.yaml", "kubectl", "get", "daemonset", "-n", namespace, "racer", "-o", "yaml")
	h.dump(ctx, "storageclasses.yaml", "kubectl", "get", "storageclasses", "-o", "yaml")
	h.dump(ctx, "pv.yaml", "kubectl", "get", "pv", "-o", "yaml")
	h.dump(ctx, "configmaps.yaml", "kubectl", "get", "configmaps", "-n", namespace, "-o", "yaml")
	h.dump(ctx, "events.txt", "kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp")

	pods, err := h.output(ctx, "kubectl", "--kubeconfig", h.kubeconfig,
		"get", "pods", "-n", namespace, "-l", "app.kubernetes.io/name=racer",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return
	}

	for _, pod := range strings.Fields(pods) {
		for _, container := range []string{"racer-ctrl", "racer", "registrar"} {
			h.dump(ctx, pod+"."+container+".log",
				"kubectl", "logs", "-n", namespace, pod, "-c", container, "--tail=400")
		}
	}
}

func (h *harness) dump(ctx context.Context, name string, command string, args ...string) {
	if command == "kubectl" {
		args = append([]string{"--kubeconfig", h.kubeconfig}, args...)
	}

	cmd := exec.CommandContext(ctx, command, args...)

	out, _ := cmd.CombinedOutput()

	_ = os.WriteFile(filepath.Join(h.artifacts, name), out, 0o600)
}
