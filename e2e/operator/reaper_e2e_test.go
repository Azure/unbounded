//go:build e2e

// Package operatore2e holds the kind-based integration test for the
// unbounded-operator legacy reaper.
//
// It is guarded by `//go:build e2e` so the default `go test ./...` skips it.
// Run via `go test -tags=e2e ./e2e/operator/...`.
//
// Rather than deploy the whole operator or net image (whose reconcilers would
// apply workloads that cannot run in vanilla kind), the test drives the real
// LegacyReaper and focused SiteController in-process against a real API server.
// The pre-redesign install is staged as inert `pause` workloads and the new
// target workloads are staged Ready, so the migration choreography (Site
// translation, secret/configmap copy, cluster-scoped ref rewrite, health-gated
// reaping, and namespace + CRD teardown) is validated against real API
// semantics (finalizers, cascading namespace deletion, DaemonSet readiness).
package operatore2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netcontroller "github.com/Azure/unbounded/internal/net/controller"
	"github.com/Azure/unbounded/internal/operator"
)

const (
	clusterName = "operator-reaper-e2e"
	pauseImage  = "registry.k8s.io/pause:3.10"
	targetNS    = "unbounded-system"
	legacyKube  = "unbounded-kube"
	legacyNet   = "unbounded-net"
	repairSite  = "kind-repair"
)

var (
	legacySiteGVK      = schema.GroupVersionKind{Group: "net.unbounded-cloud.io", Version: "v1alpha1", Kind: "Site"}
	machinaSiteGVK     = schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "Site"}
	machineGVK         = schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "Machine"}
	siteNodeSliceGVK   = schema.GroupVersionKind{Group: "net.unbounded-cloud.io", Version: "v1alpha1", Kind: "SiteNodeSlice"}
	legacySiteCRD      = "sites.net.unbounded-cloud.io"
	machinaSiteCRD     = "sites.unbounded-cloud.io"
	machineCRDName     = "machines.unbounded-cloud.io"
	siteNodeSliceCRD   = "sitenodeslices.net.unbounded-cloud.io"
	gatewayPoolCRDName = "gatewaypools.net.unbounded-cloud.io"
	legacySiteCRDYAML  = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: sites.net.unbounded-cloud.io
spec:
  group: net.unbounded-cloud.io
  scope: Cluster
  names:
    plural: sites
    singular: site
    kind: Site
    listKind: SiteList
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              x-kubernetes-preserve-unknown-fields: true
            status:
              type: object
              x-kubernetes-preserve-unknown-fields: true
`
)

type siteNodeSliceRepairFixture struct {
	siteName        string
	sliceName       string
	legacySiteUID   string
	clusterNodeCIDR string
	nodes           []any
}

func TestOperatorReaperMigration(t *testing.T) {
	requireBins(t, "kind", "kubectl", "docker")

	if err := run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("docker engine unreachable (%v); skipping suite", err)
	}

	repoRoot := repoRootFromWD(t)
	kubeconfig := createCluster(t)

	applyCRDs(t, kubeconfig, repoRoot)

	cli := newClient(t, kubeconfig)
	ctx := log.IntoContext(context.Background(), logr.FromSlogHandler(slog.Default().Handler()))

	stageNamespaces(ctx, t, cli)
	stageLegacyWorkloads(ctx, t, cli)
	stageLegacyState(ctx, t, cli)
	repairFixture := stageLegacySites(ctx, t, cli)
	stageMachine(ctx, t, cli)
	stageTargetWorkloads(ctx, t, cli)
	restrictedConfig := stageRestrictedSiteControllerIdentity(ctx, t, kubeconfig, cli)
	assertRestrictedSiteControllerPermissions(ctx, t, kubeconfig)

	reaper := &operator.LegacyReaper{
		Client:           cli,
		APIReader:        cli,
		TargetNamespace:  targetNS,
		LegacyNamespaces: operator.LegacyNamespaces,
		SkipSecretNames:  map[string]struct{}{"unbounded-net-serving-cert": {}},
		CopyConfigMaps:   []string{"machina-config", "unbounded-net-config"},
		Interval:         2 * time.Second,
	}

	runCtx, cancelRun := context.WithTimeout(ctx, 4*time.Minute)
	defer cancelRun()

	// Run one blocked phase first. The target workloads carry the hash of their
	// staged default configs, so copying the legacy payload must make those hashes
	// stale and leave the legacy net workload intact.
	stepCtx, cancelStep := context.WithCancel(runCtx)
	stepResult := make(chan error, 1)

	go func() {
		stepResult <- reaper.RunToCompletion(stepCtx)
	}()

	waitForMigratedPayloads(runCtx, t, cli)
	assertStateMigrated(runCtx, t, cli)
	assertMachineRefRewritten(runCtx, t, cli)
	assertNetBlockedOnOldHash(runCtx, t, cli)
	cancelStep()

	if err := <-stepResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked reaper phase = %v, want context cancellation", err)
	}

	updateTargetConfigHashes(runCtx, t, cli)

	reaperResult := make(chan error, 1)

	go func() {
		reaperResult <- reaper.RunToCompletion(runCtx)
	}()

	waitForReaperBlockedOnStaleSlice(runCtx, t, cli, repairFixture, reaperResult)

	controllerCtx, cancelController := context.WithCancel(runCtx)
	controllerResult := startRestrictedSiteController(controllerCtx, t, restrictedConfig)
	waitForCurrentSliceOwner(runCtx, t, cli, repairFixture, controllerResult)
	cancelController()

	select {
	case err := <-controllerResult:
		if err != nil {
			t.Fatalf("restricted SiteController stopped with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restricted SiteController did not stop")
	}

	if err := <-reaperResult; err != nil {
		t.Fatalf("reaper RunToCompletion: %v", err)
	}

	assertTranslatedSites(ctx, t, cli, repairFixture)
	assertLegacyReaped(ctx, t, cli)
	assertRepairSliceSurvives(ctx, t, cli, repairFixture)
}

// ---------------------------------------------------------------------------
// staging
// ---------------------------------------------------------------------------

func stageNamespaces(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	for _, name := range []string{targetNS, legacyKube, legacyNet} {
		mustCreate(ctx, t, cli, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
}

func stageLegacyWorkloads(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	// Legacy workloads only need to EXIST to have a footprint; keep them inert
	// (0 replicas / unschedulable DaemonSets) so kind stays light.
	mustCreate(ctx, t, cli, inertDeployment(legacyKube, "machina-controller", map[string]string{"app": "machina-controller"}))
	metalman := inertDeployment(legacyKube, "metalman-controller-edge", map[string]string{"app": "unbounded-pxe", unboundedv1alpha3.MachineSiteLabelKey: "edge"})
	metalman.Spec.Template.Spec.Containers[0].Args = []string{"serve-pxe", "--site=edge", "--dhcp-auto-interface"}
	mustCreate(ctx, t, cli, metalman)
	mustCreate(ctx, t, cli, unschedulableDaemonSet(legacyKube, "unbounded-storage-supervisor", map[string]string{"app.kubernetes.io/name": "unbounded-storage-supervisor"}))
	mustCreate(ctx, t, cli, inertDeployment(legacyNet, "unbounded-net-controller", map[string]string{"app.kubernetes.io/name": "unbounded-net-controller"}))
	mustCreate(ctx, t, cli, unschedulableDaemonSet(legacyNet, "unbounded-net-node", map[string]string{"app.kubernetes.io/name": "unbounded-net-node"}))
}

func stageLegacyState(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	mustCreate(ctx, t, cli, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "redfish-password"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	})
	// A regenerable secret that must NOT be copied.
	mustCreate(ctx, t, cli, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyNet, Name: "unbounded-net-serving-cert"},
		Data:       map[string][]byte{"tls.crt": []byte("x")},
	})
	mustCreate(ctx, t, cli, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "machina-config"},
		Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://api.example:6443"},
	})
	mustCreate(ctx, t, cli, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyNet, Name: "unbounded-net-config"},
		Data:       map[string]string{"config.yaml": "sentinel: legacy-net-config", "LOG_LEVEL": "7"},
		BinaryData: map[string][]byte{"routes.bin": {0, 1, 2}},
	})
	mustCreate(ctx, t, cli, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "unbounded-storage-config"},
		Data:       map[string]string{"config.yaml": "log_level: info"},
	})
}

func stageLegacySites(ctx context.Context, t *testing.T, cli client.Client) siteNodeSliceRepairFixture {
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

	nodeIP := net.ParseIP(internalIPs[0])
	if nodeIP == nil {
		t.Fatalf("kind node InternalIP %q is invalid", internalIPs[0])
	}

	bits := 128
	if nodeIP.To4() != nil {
		bits = 32
	}

	nodeCIDRs := make([]string, 0, 2)

	for _, candidate := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "100.64.0.0/24"} {
		_, cidr, err := net.ParseCIDR(candidate)
		if err != nil {
			t.Fatalf("parse test CIDR %q: %v", candidate, err)
		}

		if !cidr.Contains(nodeIP) {
			nodeCIDRs = append(nodeCIDRs, candidate)
		}

		if len(nodeCIDRs) == 2 {
			break
		}
	}

	if len(nodeCIDRs) != 2 {
		t.Fatalf("could not select non-overlapping Site CIDRs for kind node IP %s", nodeIP)
	}

	mustCreate(ctx, t, cli, legacySite("cluster", nodeCIDRs[0], "10.244.0.0/16"))
	mustCreate(ctx, t, cli, legacySite("edge", nodeCIDRs[1], "10.245.0.0/16"))

	repair := legacySite(repairSite, fmt.Sprintf("%s/%d", nodeIP.String(), bits), "10.246.0.0/16")
	repair.Object["spec"] = map[string]any{
		"nodeCidrs":       []any{fmt.Sprintf("%s/%d", nodeIP.String(), bits)},
		"manageCniPlugin": false,
		"podCidrAssignments": []any{
			map[string]any{"cidrBlocks": []any{"10.246.0.0/16"}, "assignmentEnabled": false},
		},
	}
	mustCreate(ctx, t, cli, repair)

	nodeData := map[string]any{
		"name":        node.Name,
		"internalIPs": stringSliceToAny(internalIPs),
	}
	if len(node.Spec.PodCIDRs) > 0 {
		nodeData["podCIDRs"] = stringSliceToAny(node.Spec.PodCIDRs)
	}

	if publicKey := node.Annotations[netcontroller.WireGuardPubKeyAnnotation]; publicKey != "" {
		nodeData["wireGuardPublicKey"] = publicKey
	}

	controllerRef := true
	blockOwnerDeletion := true
	slice := &unstructured.Unstructured{}
	slice.SetGroupVersionKind(siteNodeSliceGVK)
	slice.SetName(repairSite + "-0")
	slice.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         legacySiteGVK.GroupVersion().String(),
		Kind:               legacySiteGVK.Kind,
		Name:               repair.GetName(),
		UID:                repair.GetUID(),
		Controller:         &controllerRef,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}})
	slice.Object["siteName"] = repair.GetName()
	slice.Object["sliceIndex"] = int64(0)
	slice.Object["nodes"] = []any{nodeData}
	slice.Object["nodeCount"] = int64(1)
	mustCreate(ctx, t, cli, slice)

	return siteNodeSliceRepairFixture{
		siteName:        repair.GetName(),
		sliceName:       slice.GetName(),
		legacySiteUID:   string(repair.GetUID()),
		clusterNodeCIDR: nodeCIDRs[0],
		nodes:           []any{nodeData},
	}
}

func stageMachine(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)
	m.SetName("m1")
	mustSetNested(t, m, "example/pxe-image:v1", "spec", "pxe", "image")
	mustSetNested(t, m, "https://bmc.example", "spec", "pxe", "redfish", "url")
	mustSetNested(t, m, "admin", "spec", "pxe", "redfish", "username")
	mustSetNested(t, m, legacyKube, "spec", "pxe", "redfish", "passwordRef", "namespace")
	mustSetNested(t, m, "redfish-password", "spec", "pxe", "redfish", "passwordRef", "name")
	// Cloud-init user-data ConfigMap ref in a legacy namespace: the reaper must
	// copy the ConfigMap into the target and repoint this ref.
	mustSetNested(t, m, "cloud-init-user-data", "spec", "pxe", "cloudInit", "userDataConfigMapRef", "name")
	mustSetNested(t, m, legacyKube, "spec", "pxe", "cloudInit", "userDataConfigMapRef", "namespace")
	mustCreate(ctx, t, cli, m)

	mustCreate(ctx, t, cli, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "cloud-init-user-data"},
		Data:       map[string]string{"user-data": "#cloud-config"},
	})
}

func stageTargetWorkloads(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	// New target workloads run as pause pods so they actually become Ready in
	// kind, satisfying the reaper's per-component health gate.
	machinaConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: "machina-config"},
		Data:       map[string]string{"config.yaml": "embedded: default"},
	}
	mustCreate(ctx, t, cli, machinaConfig)

	machina := readyDeployment(targetNS, "machina-controller")
	machina.Spec.Template.Annotations = map[string]string{
		"unbounded-cloud.io/machina-config-hash": configMapHash(machinaConfig),
	}
	mustCreate(ctx, t, cli, machina)

	netConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: "unbounded-net-config"},
		Data:       map[string]string{"config.yaml": "embedded: default"},
	}
	mustCreate(ctx, t, cli, netConfig)

	netController := readyDeployment(targetNS, "unbounded-net-controller")
	netController.Spec.Template.Annotations = map[string]string{"unbounded-cloud.io/net-config-hash": configMapHash(netConfig)}
	netNode := readyDaemonSet(targetNS, "unbounded-net-node")
	netNode.Spec.Template.Annotations = map[string]string{"unbounded-cloud.io/net-config-hash": configMapHash(netConfig)}

	mustCreate(ctx, t, cli, netController)
	mustCreate(ctx, t, cli, netNode)
	mustCreate(ctx, t, cli, readyDaemonSet(targetNS, "unbounded-storage-supervisor-cluster"))
	mustCreate(ctx, t, cli, readyDaemonSet(targetNS, "unbounded-storage-supervisor-edge"))
	mustCreate(ctx, t, cli, readyDaemonSet(targetNS, "unbounded-storage-supervisor-"+repairSite))
	// The per-site metalman replacement must exist before the legacy metalman is
	// reaped (presence gate; metalman is hostNetwork like net and cannot become
	// Ready until the legacy one frees the host ports).
	mustCreate(ctx, t, cli, readyDeployment(targetNS, "metalman-controller-edge"))
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

func assertTranslatedSites(ctx context.Context, t *testing.T, cli client.Client, fixture siteNodeSliceRepairFixture) {
	t.Helper()

	cluster := getMachinaSite(ctx, t, cli, "cluster")
	if cidrs, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "nodeCidrs"); len(cidrs) != 1 || cidrs[0] != fixture.clusterNodeCIDR {
		t.Fatalf("cluster site nodeCidrs not preserved: %#v", cidrs)
	}

	if !nestedBool(cluster, "spec", "components", "machina", "enabled") {
		t.Fatalf("expected machina enabled on cluster site")
	}

	if !nestedBool(cluster, "spec", "components", "storage", "enabled") {
		t.Fatalf("expected storage enabled on cluster site")
	}

	edge := getMachinaSite(ctx, t, cli, "edge")
	if nestedBool(edge, "spec", "components", "machina", "enabled") {
		t.Fatalf("did not expect machina enabled on edge site")
	}

	if !nestedBool(edge, "spec", "components", "storage", "enabled") {
		t.Fatalf("expected storage enabled on edge site")
	}

	if !nestedBool(edge, "spec", "components", "metalman", "enabled") {
		t.Fatalf("expected metalman enabled on edge site")
	}

	if !nestedBool(edge, "spec", "components", "metalman", "dhcpAutoInterface") {
		t.Fatalf("expected Metalman DHCP auto-interface mode preserved")
	}

	repair := getMachinaSite(ctx, t, cli, fixture.siteName)

	manageCNI, found, err := unstructured.NestedBool(repair.Object, "spec", "manageCniPlugin")
	if err != nil || !found || manageCNI {
		t.Fatalf("repair Site manageCniPlugin not preserved: found=%t value=%t err=%v", found, manageCNI, err)
	}
}

func assertStateMigrated(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	var secret corev1.Secret
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "redfish-password"}, &secret); err != nil {
		t.Fatalf("expected redfish-password copied to target: %v", err)
	}

	if string(secret.Data["password"]) != "hunter2" {
		t.Fatalf("secret data not preserved: %q", secret.Data["password"])
	}

	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-net-serving-cert"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("regenerable serving cert must NOT be copied, err=%v", err)
	}

	// machina-config is copied by name; storage config is copied into the
	// operator-managed per-site ConfigMaps that storage DaemonSets mount.
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "machina-config"}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("expected machina-config copied to target: %v", err)
	}

	var netConfig corev1.ConfigMap
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-net-config"}, &netConfig); err != nil {
		t.Fatalf("expected unbounded-net-config copied to target: %v", err)
	}

	if netConfig.Data["config.yaml"] != "sentinel: legacy-net-config" || netConfig.Data["LOG_LEVEL"] != "7" || string(netConfig.BinaryData["routes.bin"]) != string([]byte{0, 1, 2}) {
		t.Fatalf("net config payload not preserved: data=%#v binaryData=%#v", netConfig.Data, netConfig.BinaryData)
	}

	for _, name := range []string{"unbounded-storage-config-cluster", "unbounded-storage-config-edge"} {
		var cm corev1.ConfigMap
		if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: name}, &cm); err != nil {
			t.Fatalf("expected per-site storage config %s copied to target: %v", name, err)
		}

		if cm.Data["config.yaml"] != "log_level: info" {
			t.Fatalf("storage config %s data not preserved: %q", name, cm.Data["config.yaml"])
		}
	}

	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-storage-config"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("shared storage config must NOT be copied under the legacy name, err=%v", err)
	}

	// The Machine cloud-init ConfigMap is copied out of the legacy namespace.
	var cloudInit corev1.ConfigMap
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "cloud-init-user-data"}, &cloudInit); err != nil {
		t.Fatalf("expected cloud-init configmap copied to target: %v", err)
	}

	if cloudInit.Data["user-data"] != "#cloud-config" {
		t.Fatalf("cloud-init configmap data not preserved: %q", cloudInit.Data["user-data"])
	}
}

func assertNetBlockedOnOldHash(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	var config corev1.ConfigMap
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-net-config"}, &config); err != nil {
		t.Fatalf("get migrated net config: %v", err)
	}

	var deploy appsv1.Deployment
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-net-controller"}, &deploy); err != nil {
		t.Fatalf("get target net controller: %v", err)
	}

	if deploy.Spec.Template.Annotations["unbounded-cloud.io/net-config-hash"] == configMapHash(&config) {
		t.Fatal("target net controller unexpectedly carried the migrated config hash before rollout")
	}

	if err := cli.Get(ctx, client.ObjectKey{Namespace: legacyNet, Name: "unbounded-net-controller"}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("legacy net controller was reaped with a stale target config hash: %v", err)
	}
}

func updateTargetConfigHashes(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	updateDeploymentHash(ctx, t, cli, "machina-controller", "machina-config", "unbounded-cloud.io/machina-config-hash")
	updateDeploymentHash(ctx, t, cli, "unbounded-net-controller", "unbounded-net-config", "unbounded-cloud.io/net-config-hash")
	updateDaemonSetHash(ctx, t, cli, "unbounded-net-node", "unbounded-net-config", "unbounded-cloud.io/net-config-hash")

	for _, site := range []string{"cluster", "edge", repairSite} {
		updateDaemonSetHash(ctx, t, cli,
			"unbounded-storage-supervisor-"+site,
			"unbounded-storage-config-"+site,
			"unbounded-cloud.io/storage-config-hash")
	}
}

func updateDeploymentHash(ctx context.Context, t *testing.T, cli client.Client, workload, configName, annotation string) {
	t.Helper()

	var config corev1.ConfigMap
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: configName}, &config); err != nil {
		t.Fatalf("get target config %s: %v", configName, err)
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var deploy appsv1.Deployment
		if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: workload}, &deploy); err != nil {
			return err
		}

		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = map[string]string{}
		}

		deploy.Spec.Template.Annotations[annotation] = configMapHash(&config)

		return cli.Update(ctx, &deploy)
	})
	if err != nil {
		t.Fatalf("update target Deployment %s hash: %v", workload, err)
	}
}

func updateDaemonSetHash(ctx context.Context, t *testing.T, cli client.Client, workload, configName, annotation string) {
	t.Helper()

	var config corev1.ConfigMap
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: configName}, &config); err != nil {
		t.Fatalf("get target config %s: %v", configName, err)
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var daemonSet appsv1.DaemonSet
		if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: workload}, &daemonSet); err != nil {
			return err
		}

		if daemonSet.Spec.Template.Annotations == nil {
			daemonSet.Spec.Template.Annotations = map[string]string{}
		}

		daemonSet.Spec.Template.Annotations[annotation] = configMapHash(&config)

		return cli.Update(ctx, &daemonSet)
	})
	if err != nil {
		t.Fatalf("update target DaemonSet %s hash: %v", workload, err)
	}
}

func waitForMigratedPayloads(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		var netConfig corev1.ConfigMap

		netErr := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-net-config"}, &netConfig)

		var clusterStorage corev1.ConfigMap

		clusterStorageErr := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-storage-config-cluster"}, &clusterStorage)

		var edgeStorage corev1.ConfigMap

		edgeStorageErr := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-storage-config-edge"}, &edgeStorage)

		var repairStorage corev1.ConfigMap

		repairStorageErr := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-storage-config-" + repairSite}, &repairStorage)
		if netErr == nil && clusterStorageErr == nil && edgeStorageErr == nil && repairStorageErr == nil &&
			netConfig.Data["config.yaml"] == "sentinel: legacy-net-config" &&
			clusterStorage.Data["config.yaml"] == "log_level: info" &&
			edgeStorage.Data["config.yaml"] == "log_level: info" &&
			repairStorage.Data["config.yaml"] == "log_level: info" {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("wait for migrated payloads: %v (net=%v cluster storage=%v edge storage=%v repair storage=%v)",
				ctx.Err(), netErr, clusterStorageErr, edgeStorageErr, repairStorageErr)
		case <-ticker.C:
		}
	}
}

func waitForReaperBlockedOnStaleSlice(
	ctx context.Context,
	t *testing.T,
	cli client.Client,
	fixture siteNodeSliceRepairFixture,
	reaperResult <-chan error,
) {
	t.Helper()

	err := utilwait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 45*time.Second, true, func(ctx context.Context) (bool, error) {
		select {
		case err := <-reaperResult:
			return false, fmt.Errorf("reaper completed before stale slice repair: %w", err)
		default:
		}

		for _, namespace := range []string{legacyKube, legacyNet} {
			err := cli.Get(ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{})
			if err == nil {
				return false, nil
			}

			if !apierrors.IsNotFound(err) {
				return false, err
			}
		}

		if err := cli.Get(ctx, client.ObjectKey{Name: legacySiteCRD}, &apiextensionsv1.CustomResourceDefinition{}); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Errorf("legacy Site CRD was deleted while stale slice ownership remained")
			}

			return false, err
		}

		slice := &unstructured.Unstructured{}
		slice.SetGroupVersionKind(siteNodeSliceGVK)

		if err := cli.Get(ctx, client.ObjectKey{Name: fixture.sliceName}, slice); err != nil {
			return false, err
		}

		refs := slice.GetOwnerReferences()
		if len(refs) != 1 ||
			refs[0].APIVersion != legacySiteGVK.GroupVersion().String() ||
			refs[0].Kind != legacySiteGVK.Kind ||
			refs[0].Name != fixture.siteName ||
			string(refs[0].UID) != fixture.legacySiteUID ||
			refs[0].Controller == nil || !*refs[0].Controller ||
			refs[0].BlockOwnerDeletion == nil || !*refs[0].BlockOwnerDeletion {
			return false, fmt.Errorf("staged legacy owner changed before controller start: %#v", refs)
		}

		return true, nil
	})
	if err != nil {
		t.Fatalf("wait for reaper to reach stale-owner cleanup gate: %v", err)
	}
}

func waitForCurrentSliceOwner(
	ctx context.Context,
	t *testing.T,
	cli client.Client,
	fixture siteNodeSliceRepairFixture,
	controllerResult <-chan error,
) {
	t.Helper()

	var lastRefs []metav1.OwnerReference

	err := utilwait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		select {
		case err := <-controllerResult:
			return false, fmt.Errorf("restricted SiteController exited before owner repair: %w", err)
		default:
		}

		site := &unstructured.Unstructured{}
		site.SetGroupVersionKind(machinaSiteGVK)

		if err := cli.Get(ctx, client.ObjectKey{Name: fixture.siteName}, site); err != nil {
			return false, client.IgnoreNotFound(err)
		}

		slice := &unstructured.Unstructured{}
		slice.SetGroupVersionKind(siteNodeSliceGVK)

		if err := cli.Get(ctx, client.ObjectKey{Name: fixture.sliceName}, slice); err != nil {
			return false, err
		}

		lastRefs = slice.GetOwnerReferences()
		if !hasExactCurrentSiteOwner(lastRefs, site) {
			return false, nil
		}

		nodes, found, err := unstructured.NestedSlice(slice.Object, "nodes")
		if err != nil {
			return false, err
		}

		return found && reflect.DeepEqual(nodes, fixture.nodes), nil
	})
	if err != nil {
		t.Fatalf("wait for restricted SiteController owner repair: %v (last owner refs: %#v)", err, lastRefs)
	}
}

func assertMachineRefRewritten(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)

	if err := cli.Get(ctx, client.ObjectKey{Name: "m1"}, m); err != nil {
		t.Fatalf("get machine: %v", err)
	}

	got, _, _ := unstructured.NestedString(m.Object, "spec", "pxe", "redfish", "passwordRef", "namespace")
	if got != targetNS {
		t.Fatalf("machine passwordRef namespace = %q, want %q", got, targetNS)
	}

	cloudInitNS, _, _ := unstructured.NestedString(m.Object, "spec", "pxe", "cloudInit", "userDataConfigMapRef", "namespace")
	if cloudInitNS != targetNS {
		t.Fatalf("machine cloud-init configmap namespace = %q, want %q", cloudInitNS, targetNS)
	}
}

func assertLegacyReaped(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	for _, name := range []string{legacyKube, legacyNet} {
		if err := cli.Get(ctx, client.ObjectKey{Name: name}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected legacy namespace %s deleted, err=%v", name, err)
		}
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := cli.Get(ctx, client.ObjectKey{Name: legacySiteCRD}, crd); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy site CRD deleted, err=%v", err)
	}
}

func assertRepairSliceSurvives(ctx context.Context, t *testing.T, cli client.Client, fixture siteNodeSliceRepairFixture) {
	t.Helper()

	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		t.Fatalf("wait for garbage collection: %v", ctx.Err())
	case <-timer.C:
	}

	site := getMachinaSite(ctx, t, cli, fixture.siteName)
	slice := &unstructured.Unstructured{}
	slice.SetGroupVersionKind(siteNodeSliceGVK)

	if err := cli.Get(ctx, client.ObjectKey{Name: fixture.sliceName}, slice); err != nil {
		t.Fatalf("repair SiteNodeSlice did not survive legacy CRD deletion: %v", err)
	}

	if !hasExactCurrentSiteOwner(slice.GetOwnerReferences(), site) {
		t.Fatalf("surviving SiteNodeSlice owner changed: %#v", slice.GetOwnerReferences())
	}
}

// ---------------------------------------------------------------------------
// object builders
// ---------------------------------------------------------------------------

func inertDeployment(namespace, name string, labels map[string]string) *appsv1.Deployment {
	zero := int32(0)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: podTemplate(labels),
		},
	}
}

func unschedulableDaemonSet(namespace, name string, labels map[string]string) *appsv1.DaemonSet {
	tmpl := podTemplate(labels)
	tmpl.Spec.NodeSelector = map[string]string{"operator-reaper-e2e/absent": "true"}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: tmpl,
		},
	}
}

func readyDeployment(namespace, name string) *appsv1.Deployment {
	one := int32(1)
	labels := map[string]string{"app.kubernetes.io/name": name}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: podTemplate(labels),
		},
	}
}

func readyDaemonSet(namespace, name string) *appsv1.DaemonSet {
	labels := map[string]string{"app.kubernetes.io/name": name}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: podTemplate(labels),
		},
	}
}

func podTemplate(labels map[string]string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "pause", Image: pauseImage}},
		},
	}
}

func legacySite(name, nodeCidr, podCidr string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(legacySiteGVK)
	obj.SetName(name)
	obj.Object["spec"] = map[string]any{
		"nodeCidrs": []any{nodeCidr},
		"podCidrAssignments": []any{
			map[string]any{"cidrBlocks": []any{podCidr}},
		},
	}

	return obj
}

func getMachinaSite(ctx context.Context, t *testing.T, cli client.Client, name string) *unstructured.Unstructured {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(machinaSiteGVK)

	if err := cli.Get(ctx, client.ObjectKey{Name: name}, obj); err != nil {
		t.Fatalf("expected translated machina site %s: %v", name, err)
	}

	return obj
}

func nestedBool(obj *unstructured.Unstructured, path ...string) bool {
	v, _, _ := unstructured.NestedBool(obj.Object, path...)

	return v
}

func hasExactCurrentSiteOwner(refs []metav1.OwnerReference, site *unstructured.Unstructured) bool {
	if len(refs) != 1 {
		return false
	}

	ref := refs[0]

	return ref.APIVersion == machinaSiteGVK.GroupVersion().String() &&
		ref.Kind == machinaSiteGVK.Kind &&
		ref.Name == site.GetName() &&
		ref.UID == site.GetUID() &&
		ref.Controller != nil && *ref.Controller &&
		ref.BlockOwnerDeletion != nil && !*ref.BlockOwnerDeletion
}

func stringSliceToAny(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}

	return result
}

func configMapHash(config *corev1.ConfigMap) string {
	payload, err := json.Marshal(struct {
		Data       map[string]string `json:"data"`
		BinaryData map[string][]byte `json:"binaryData"`
	}{Data: config.Data, BinaryData: config.BinaryData})
	if err != nil {
		panic(err)
	}

	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

// ---------------------------------------------------------------------------
// cluster + client lifecycle
// ---------------------------------------------------------------------------

func createCluster(t *testing.T) string {
	t.Helper()

	return createClusterNamed(t, clusterName)
}

func createClusterNamed(t *testing.T, name string) string {
	t.Helper()

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")

	if err := run(context.Background(), "kind", "create", "cluster", "--name", name, "--wait", "120s", "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("kind create cluster: %v", err)
	}

	t.Cleanup(func() {
		if os.Getenv("E2E_KEEP") == "1" {
			t.Logf("E2E_KEEP=1; leaving kind cluster %q", name)
			return
		}

		_ = run(context.Background(), "kind", "delete", "cluster", "--name", name)
	})

	return kubeconfig
}

func applyCRDs(t *testing.T, kubeconfig, repoRoot string) {
	t.Helper()

	crdFile := filepath.Join(t.TempDir(), "legacy-site-crd.yaml")
	if err := os.WriteFile(crdFile, []byte(legacySiteCRDYAML), 0o600); err != nil {
		t.Fatalf("write legacy site crd: %v", err)
	}

	files := []string{
		filepath.Join(repoRoot, "deploy", "machina", "crd", "unbounded-cloud.io_sites.yaml"),
		filepath.Join(repoRoot, "deploy", "machina", "crd", "unbounded-cloud.io_machines.yaml"),
		filepath.Join(repoRoot, "deploy", "net", "crd", "net.unbounded-cloud.io_sitenodeslices.yaml"),
		filepath.Join(repoRoot, "deploy", "net", "crd", "net.unbounded-cloud.io_gatewaypools.yaml"),
		crdFile,
	}

	for _, f := range files {
		if err := run(context.Background(), "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", f); err != nil {
			t.Fatalf("kubectl apply %s: %v", f, err)
		}
	}

	if err := run(context.Background(), "kubectl", "--kubeconfig", kubeconfig, "wait", "--for=condition=established", "--timeout=60s",
		"crd/"+machinaSiteCRD, "crd/"+machineCRDName, "crd/"+siteNodeSliceCRD, "crd/"+gatewayPoolCRDName, "crd/"+legacySiteCRD); err != nil {
		t.Fatalf("kubectl wait for CRDs: %v", err)
	}
}

func stageRestrictedSiteControllerIdentity(ctx context.Context, t *testing.T, kubeconfig string, cli client.Client) *rest.Config {
	t.Helper()

	const name = "reaper-site-controller"

	mustCreate(ctx, t, cli, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: name}})
	mustCreate(ctx, t, cli, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch", "update"}},
			{APIGroups: []string{"unbounded-cloud.io"}, Resources: []string{"sites"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{"unbounded-cloud.io"}, Resources: []string{"sites/status"}, Verbs: []string{"get", "patch", "update"}},
			// Legacy net-group Sites: read-only, for the SiteNodeSlice orphan
			// cleanup migration-window check (mirrors the production net RBAC).
			{APIGroups: []string{"net.unbounded-cloud.io"}, Resources: []string{"sites"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"net.unbounded-cloud.io"}, Resources: []string{"gatewaypools"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"net.unbounded-cloud.io"}, Resources: []string{"sitenodeslices"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		},
	})
	mustCreate(ctx, t, cli, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: targetNS, Name: name}},
	})

	adminConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build admin rest config: %v", err)
	}

	adminClient, err := kubernetes.NewForConfig(adminConfig)
	if err != nil {
		t.Fatalf("build admin clientset: %v", err)
	}

	expirationSeconds := int64(600)

	token, err := adminClient.CoreV1().ServiceAccounts(targetNS).CreateToken(ctx, name, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expirationSeconds},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create restricted service account token: %v", err)
	}

	restrictedConfig := rest.AnonymousClientConfig(adminConfig)
	restrictedConfig.BearerToken = token.Status.Token

	return restrictedConfig
}

func assertRestrictedSiteControllerPermissions(ctx context.Context, t *testing.T, kubeconfig string) {
	t.Helper()

	adminConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build admin rest config for access reviews: %v", err)
	}

	adminClient, err := kubernetes.NewForConfig(adminConfig)
	if err != nil {
		t.Fatalf("build admin clientset for access reviews: %v", err)
	}

	username := "system:serviceaccount:" + targetNS + ":reaper-site-controller"
	checks := []struct {
		name       string
		attributes authorizationv1.ResourceAttributes
		allowed    bool
	}{
		{
			name: "site updates",
			attributes: authorizationv1.ResourceAttributes{
				Group: "unbounded-cloud.io", Version: "v1alpha3", Resource: "sites", Verb: "update",
			},
			allowed: true,
		},
		{
			name: "site finalizer updates",
			attributes: authorizationv1.ResourceAttributes{
				Group: "unbounded-cloud.io", Version: "v1alpha3", Resource: "sites", Subresource: "finalizers", Verb: "update",
			},
			allowed: false,
		},
		{
			name: "slice updates",
			attributes: authorizationv1.ResourceAttributes{
				Group: "net.unbounded-cloud.io", Version: "v1alpha1", Resource: "sitenodeslices", Verb: "update",
			},
			allowed: true,
		},
		{
			name: "slice deletes",
			attributes: authorizationv1.ResourceAttributes{
				Group: "net.unbounded-cloud.io", Version: "v1alpha1", Resource: "sitenodeslices", Verb: "delete",
			},
			allowed: true,
		},
	}

	last := make(map[string]bool, len(checks))

	err = utilwait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		matches := true

		for _, check := range checks {
			review, err := adminClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User:               username,
					ResourceAttributes: &check.attributes,
				},
			}, metav1.CreateOptions{})
			if err != nil {
				return false, err
			}

			last[check.name] = review.Status.Allowed
			matches = matches && review.Status.Allowed == check.allowed
		}

		return matches, nil
	})
	if err != nil {
		t.Fatalf("restricted SiteController permissions did not converge: %v (last results: %#v)", err, last)
	}
}

func startRestrictedSiteController(ctx context.Context, t *testing.T, config *rest.Config) <-chan error {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("build restricted typed client: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("build restricted dynamic client: %v", err)
	}

	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	typedInformerFactory := informers.NewSharedInformerFactory(clientset, 0)

	siteController, err := netcontroller.NewSiteController(clientset, dynamicClient, dynamicInformerFactory, typedInformerFactory)
	if err != nil {
		t.Fatalf("build real SiteController: %v", err)
	}

	dynamicInformerFactory.Start(ctx.Done())
	typedInformerFactory.Start(ctx.Done())

	result := make(chan error, 1)

	go func() {
		result <- siteController.Run(ctx, 1)
	}()

	return result
}

func newClient(t *testing.T, kubeconfig string) client.Client {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build rest config: %v", err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		batchv1.AddToScheme,
		rbacv1.AddToScheme,
		apiextensionsv1.AddToScheme,
		unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	return cli
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustCreate(ctx context.Context, t *testing.T, cli client.Client, obj client.Object) {
	t.Helper()

	if err := cli.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func mustSetNested(t *testing.T, obj *unstructured.Unstructured, value string, path ...string) {
	t.Helper()

	if err := unstructured.SetNestedField(obj.Object, value, path...); err != nil {
		t.Fatalf("set nested %v: %v", path, err)
	}
}

func requireBins(t *testing.T, bins ...string) {
	t.Helper()

	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("e2e prereq %q missing on PATH; skipping suite", bin)
		}
	}
}

func repoRootFromWD(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// e2e/operator -> repo root
	return filepath.Dir(filepath.Dir(wd))
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
