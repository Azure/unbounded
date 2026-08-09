//go:build e2e

// This file holds a focused kind-based e2e that reproduces the released-cluster
// migration/upgrade window and proves the net SiteController never deletes live
// SiteNodeSlices (the source of truth for WireGuard peers and pod-CIDR routes)
// while the machina Site set is empty or only partially translated. It runs the
// real SiteController against a real API server, so it exercises informer sync,
// RBAC, and list/get semantics the fake-client unit tests cannot.
//
// Run via `go test -tags=e2e -run TestSiteControllerPreservesSlicesDuringMigrationWindow ./e2e/operator/...`.
package operatore2e

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const sliceWindowClusterName = "operator-slice-window-e2e"

// TestSiteControllerPreservesSlicesDuringMigrationWindow drives the real net
// SiteController through the migration/upgrade window against a real API server
// and asserts the dataplane's source of truth (SiteNodeSlices) is never torn
// down:
//
//   - Phase 1 (empty machina window): with legacy Sites + slices present but NO
//     machina Sites yet, the slice must remain continuously present. This is the
//     regression #1 fixes; the pre-fix controller deleted every slice here.
//   - Phase 2 (convergence): once the machina Site is created, the slice is
//     re-owned and still present.
//   - Phase 3 (partial migration): with the machina set non-empty, a slice whose
//     Site is still only in the legacy group must survive (legacySiteExists).
//   - Phase 4 (genuine orphan): a slice whose Site is absent from both groups is
//     still cleaned up, proving the guard did not neuter cleanup.
func TestSiteControllerPreservesSlicesDuringMigrationWindow(t *testing.T) {
	requireBins(t, "kind", "kubectl", "docker")

	if err := run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("docker engine unreachable (%v); skipping suite", err)
	}

	repoRoot := repoRootFromWD(t)
	kubeconfig := createClusterNamed(t, sliceWindowClusterName)

	applyCRDs(t, kubeconfig, repoRoot)

	cli := newClient(t, kubeconfig)
	ctx := log.IntoContext(context.Background(), logr.FromSlogHandler(slog.Default().Handler()))

	// The restricted SiteController identity's ServiceAccount lives in targetNS.
	mustCreate(ctx, t, cli, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNS}})
	restrictedConfig := stageRestrictedSiteControllerIdentity(ctx, t, kubeconfig, cli)

	node, internalIPs := firstKindNode(ctx, t, cli)

	nodeIP := net.ParseIP(internalIPs[0])
	if nodeIP == nil {
		t.Fatalf("kind node InternalIP %q is invalid", internalIPs[0])
	}

	bits := 128
	if nodeIP.To4() != nil {
		bits = 32
	}

	nodeCIDR := fmt.Sprintf("%s/%d", nodeIP.String(), bits)
	nodeData := sliceNodeData(node, internalIPs)

	// Stage the released-cluster state: a legacy net-group Site that owns the
	// node, plus its SiteNodeSlice. No machina Site exists yet.
	winSite := legacySite("win-site", nodeCIDR, "10.244.0.0/16")
	winSite.Object["spec"] = map[string]any{
		"nodeCidrs":       []any{nodeCIDR},
		"manageCniPlugin": false,
		"podCidrAssignments": []any{
			map[string]any{"cidrBlocks": []any{"10.244.0.0/16"}, "assignmentEnabled": false},
		},
	}
	mustCreate(ctx, t, cli, winSite)

	stageLegacyOwnedSlice(ctx, t, cli, "win-site-0", "win-site", winSite.GetUID(), nodeData)

	// Start the real SiteController with NO machina Sites present.
	controllerCtx, cancelController := context.WithCancel(ctx)
	defer cancelController()

	controllerResult := startRestrictedSiteController(controllerCtx, t, restrictedConfig)

	// Phase 1: empty machina window. The slice must never disappear while the
	// controller reconciles with zero machina Sites.
	assertSliceContinuouslyPresent(controllerCtx, t, cli, "win-site-0", 12*time.Second, controllerResult)
	assertSliceOwnerGroup(ctx, t, cli, "win-site-0", legacySiteGVK.Group)

	// Phase 2: convergence. Create the machina Site; the controller re-owns the
	// slice and keeps it.
	mustCreate(ctx, t, cli, machinaSiteObj("win-site", nodeCIDR, "10.244.0.0/16"))
	waitForMachinaSliceOwner(controllerCtx, t, cli, "win-site-0", "win-site", controllerResult)

	// Phase 3: partial migration. A legacy-only Site's slice must survive while
	// the machina set is non-empty (exercises the legacy-group check).
	edgeSite := legacySite("edge-site", "192.0.2.0/24", "10.245.0.0/16")
	mustCreate(ctx, t, cli, edgeSite)
	stageLegacyOwnedSlice(ctx, t, cli, "edge-site-0", "edge-site", edgeSite.GetUID(), nodeData)
	assertSliceContinuouslyPresent(controllerCtx, t, cli, "edge-site-0", 8*time.Second, controllerResult)

	// Phase 4: genuine orphan. A slice whose Site exists in neither group must be
	// deleted, proving the guard did not disable cleanup.
	stageLegacyOwnedSlice(ctx, t, cli, "ghost-0", "ghost", "", nodeData)
	waitForSliceDeleted(controllerCtx, t, cli, "ghost-0", controllerResult)

	cancelController()

	select {
	case err := <-controllerResult:
		if err != nil {
			t.Fatalf("restricted SiteController stopped with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restricted SiteController did not stop")
	}
}

// firstKindNode returns the single kind node and its InternalIPs.
func firstKindNode(ctx context.Context, t *testing.T, cli client.Client) (*corev1.Node, []string) {
	t.Helper()

	var nodeList corev1.NodeList
	if err := cli.List(ctx, &nodeList); err != nil {
		t.Fatalf("list kind nodes: %v", err)
	}

	if len(nodeList.Items) != 1 {
		t.Fatalf("kind node count = %d, want 1", len(nodeList.Items))
	}

	node := &nodeList.Items[0]

	internalIPs := make([]string, 0, len(node.Status.Addresses))

	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			internalIPs = append(internalIPs, address.Address)
		}
	}

	if len(internalIPs) == 0 {
		t.Fatalf("kind node %s has no InternalIP", node.Name)
	}

	return node, internalIPs
}

// sliceNodeData builds the SiteNodeSlice node entry for the kind node.
func sliceNodeData(node *corev1.Node, internalIPs []string) map[string]any {
	nodeData := map[string]any{
		"name":        node.Name,
		"internalIPs": stringSliceToAny(internalIPs),
	}
	if len(node.Spec.PodCIDRs) > 0 {
		nodeData["podCIDRs"] = stringSliceToAny(node.Spec.PodCIDRs)
	}

	return nodeData
}

// machinaSiteObj builds a machina-group Site that node-selects the kind node.
func machinaSiteObj(name, nodeCidr, podCidr string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(machinaSiteGVK)
	obj.SetName(name)
	obj.Object["spec"] = map[string]any{
		"nodeCidrs":       []any{nodeCidr},
		"manageCniPlugin": false,
		"podCidrAssignments": []any{
			map[string]any{"cidrBlocks": []any{podCidr}, "assignmentEnabled": false},
		},
	}

	return obj
}

// stageLegacyOwnedSlice creates a SiteNodeSlice referencing siteName. When
// siteUID is non-empty the slice carries a controller owner reference to the
// legacy Site (the released-cluster shape); an empty UID leaves it ownerless so
// garbage collection does not race the orphan-cleanup assertion.
func stageLegacyOwnedSlice(ctx context.Context, t *testing.T, cli client.Client, sliceName, siteName string, siteUID types.UID, nodeData map[string]any) {
	t.Helper()

	slice := &unstructured.Unstructured{}
	slice.SetGroupVersionKind(siteNodeSliceGVK)
	slice.SetName(sliceName)

	if siteUID != "" {
		controllerRef := true
		blockOwnerDeletion := true
		slice.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion:         legacySiteGVK.GroupVersion().String(),
			Kind:               legacySiteGVK.Kind,
			Name:               siteName,
			UID:                siteUID,
			Controller:         &controllerRef,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}})
	}

	slice.Object["siteName"] = siteName
	slice.Object["sliceIndex"] = int64(0)
	slice.Object["nodes"] = []any{nodeData}
	slice.Object["nodeCount"] = int64(1)

	mustCreate(ctx, t, cli, slice)
}

// assertSliceContinuouslyPresent samples the slice every 250ms for the given
// duration and fails if it is ever observed absent (or the controller exits).
func assertSliceContinuouslyPresent(ctx context.Context, t *testing.T, cli client.Client, sliceName string, duration time.Duration, controllerResult <-chan error) {
	t.Helper()

	deadline := time.Now().Add(duration)
	samples := 0

	for time.Now().Before(deadline) {
		select {
		case err := <-controllerResult:
			t.Fatalf("SiteController exited during continuity window: %v", err)
		default:
		}

		slice := &unstructured.Unstructured{}
		slice.SetGroupVersionKind(siteNodeSliceGVK)

		if err := cli.Get(ctx, client.ObjectKey{Name: sliceName}, slice); err != nil {
			t.Fatalf("slice %s disappeared during migration window (sample %d): %v", sliceName, samples, err)
		}

		samples++

		time.Sleep(250 * time.Millisecond)
	}

	if samples == 0 {
		t.Fatalf("continuity window produced no samples for %s", sliceName)
	}

	t.Logf("slice %s stayed present across %d samples over %s", sliceName, samples, duration)
}

// assertSliceOwnerGroup asserts the slice's sole owner reference is in the given
// API group.
func assertSliceOwnerGroup(ctx context.Context, t *testing.T, cli client.Client, sliceName, group string) {
	t.Helper()

	slice := &unstructured.Unstructured{}
	slice.SetGroupVersionKind(siteNodeSliceGVK)

	if err := cli.Get(ctx, client.ObjectKey{Name: sliceName}, slice); err != nil {
		t.Fatalf("get slice %s: %v", sliceName, err)
	}

	refs := slice.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("slice %s owner refs = %#v, want exactly one", sliceName, refs)
	}

	if gv := refs[0].APIVersion; gv != group+"/v1alpha1" && gv != group+"/v1alpha3" {
		t.Fatalf("slice %s owner group = %q, want %q", sliceName, gv, group)
	}
}

// waitForMachinaSliceOwner waits until the slice is re-owned by the machina Site
// and is still present.
func waitForMachinaSliceOwner(ctx context.Context, t *testing.T, cli client.Client, sliceName, siteName string, controllerResult <-chan error) {
	t.Helper()

	err := utilwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		select {
		case err := <-controllerResult:
			return false, fmt.Errorf("SiteController exited before re-owning slice: %w", err)
		default:
		}

		site := getMachinaSite(ctx, t, cli, siteName)

		slice := &unstructured.Unstructured{}
		slice.SetGroupVersionKind(siteNodeSliceGVK)

		if err := cli.Get(ctx, client.ObjectKey{Name: sliceName}, slice); err != nil {
			return false, err
		}

		return hasExactCurrentSiteOwner(slice.GetOwnerReferences(), site), nil
	})
	if err != nil {
		t.Fatalf("wait for machina owner on slice %s: %v", sliceName, err)
	}
}

// waitForSliceDeleted waits until the slice is deleted by the controller.
func waitForSliceDeleted(ctx context.Context, t *testing.T, cli client.Client, sliceName string, controllerResult <-chan error) {
	t.Helper()

	err := utilwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		select {
		case err := <-controllerResult:
			return false, fmt.Errorf("SiteController exited before deleting orphan slice: %w", err)
		default:
		}

		slice := &unstructured.Unstructured{}
		slice.SetGroupVersionKind(siteNodeSliceGVK)

		err := cli.Get(ctx, client.ObjectKey{Name: sliceName}, slice)
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		return false, client.IgnoreNotFound(err)
	})
	if err != nil {
		t.Fatalf("wait for orphan slice %s deletion: %v", sliceName, err)
	}
}
