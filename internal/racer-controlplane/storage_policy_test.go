// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer"
)

type storageAPI struct {
	client.Client
	writes          int
	fail, ambiguous bool
}

func (c *storageAPI) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		c.writes++
	}

	return c.Client.Create(ctx, obj, opts...)
}

func (c *storageAPI) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.writes++
	if c.fail {
		return errors.New("storage unavailable")
	}

	err := c.Client.Update(ctx, obj, opts...)
	if c.ambiguous && err == nil {
		return errors.New("lost update response")
	}

	return err
}

func newStorageTest(t *testing.T, c client.Client, s *Server) *storageReconciler {
	t.Helper()

	if err := machina.AddToScheme(c.Scheme()); err != nil {
		t.Fatal(err)
	}

	return &storageReconciler{client: c, inventory: c, store: stateStore{client: c, namespace: "state"}, server: s}
}

func TestStorageInheritanceRestartAndTopologyIsolation(t *testing.T) {
	ctx := context.Background()
	n, p, svc := fixtures()
	n2, p2 := n.DeepCopy(), p.DeepCopy()
	n2.Name, n2.UID = "second", "second-uid"
	p2.Name, p2.Spec.NodeName, p2.Status.PodIP = "second", n2.Name, "10.1.1.2"
	c := fakeKube(n, p, n2, p2, svc)
	topology := newTestReconciler(c)
	api := &storageAPI{Client: c}
	r := newStorageTest(t, api, topology.server)
	quantity := resource.MustParse("12Gi")
	site := &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	if err := c.Get(ctx, client.ObjectKeyFromObject(site), site); err != nil {
		t.Fatal(err)
	}

	site.Spec.Components.Racer.CacheSize = &quantity
	if err := c.Update(ctx, site); err != nil {
		t.Fatal(err)
	}

	reconcileNode := func(node *corev1.Node) storagePolicyRecord {
		t.Helper()

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: node.Name}}); err != nil {
			t.Fatal(err)
		}

		if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
			t.Fatal(err)
		}

		return r.server.storagePolicies[identity("node", string(node.UID))]
	}
	topologyStep := func() {
		t.Helper()

		if _, err := topology.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
			t.Fatal(err)
		}
	}
	topologyStep()

	before, _ := indexGeneration(topology.loaded["default"])
	first := reconcileNode(n)
	second := reconcileNode(n2)

	if first.DesiredBytes != 12<<30 || first.Version != 1 {
		t.Fatalf("inherit: %+v", first)
	}

	if got := r.siteRequests(ctx, site); len(got) != 2 {
		t.Fatalf("Site mapping: %v", got)
	}
	// Canonical presence suppresses deprecated membership, including empty.
	n2.Labels[racer.SiteLabelKey] = ""

	n2.Labels[racer.DeprecatedSiteLabelKey] = "default"
	if err := c.Update(ctx, n2); err != nil {
		t.Fatal(err)
	}

	if got := r.siteRequests(ctx, site); len(got) != 1 || got[0].Name != n.Name {
		t.Fatalf("canonical mapping: %v", got)
	}

	n2.Labels[racer.SiteLabelKey] = "default"
	if err := c.Update(ctx, n2); err != nil {
		t.Fatal(err)
	}

	n.Annotations = map[string]string{racer.CacheSizeAnnotationKey: "20Gi"}
	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	override := reconcileNode(n)

	topologyStep()

	after, _ := indexGeneration(topology.loaded["default"])
	for _, member := range before.g.Nodes {
		if !proto.Equal(before.snapshot(member.ID), after.snapshot(member.ID)) {
			t.Fatal("cache edit changed snapshot/epoch")
		}
	}

	if override.Version != 2 || override.DesiredBytes != 20<<30 || reconcileNode(n2) != second {
		t.Fatal("override affected another node")
	}

	quantity = resource.MustParse("16Gi")

	if err := c.Update(ctx, site); err != nil {
		t.Fatal(err)
	}

	if reconcileNode(n) != override || reconcileNode(n2).DesiredBytes != 16<<30 {
		t.Fatal("Site inheritance ignored override")
	}

	delete(n.Annotations, racer.CacheSizeAnnotationKey)

	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	inherited := reconcileNode(n)
	if inherited.DesiredBytes != 16<<30 || inherited.Version != 3 {
		t.Fatalf("override removal: %+v", inherited)
	}

	n.Annotations = map[string]string{racer.CacheSizeAnnotationKey: ""}
	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	invalid := reconcileNode(n)
	if invalid.ValidationError == "" || invalid.DesiredBytes != inherited.DesiredBytes || invalid.Version != inherited.Version {
		t.Fatal("invalid override lost prior desired")
	}
	// Invalid storage input must not prevent an independent topology edit.
	// Complete the existing topology barrier before proposing another generation.
	roll := topology.server.rollouts["default"]
	if err := topology.server.persistPhase(ctx, "default", roll, 4); err != nil {
		t.Fatal(err)
	}

	for _, member := range before.g.Nodes {
		roll.acks[member.ID] = rolloutAck{boot: strings.Repeat("ab", 32), phase: 4, seen: time.Now()}
	}

	n.Annotations[racer.FabricAnnotationKey] = "changed"
	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	topologyStep()

	if topology.loaded["default"].Revision <= before.g.Revision {
		t.Fatal("invalid storage blocked topology")
	}

	writes := api.writes

	reconcileNode(n)

	r = newStorageTest(t, api, &Server{})
	if got := reconcileNode(n); got != invalid {
		t.Fatalf("restart changed policy: %+v", got)
	}

	if api.writes != writes {
		t.Fatal("no-op/restart wrote durable state")
	}

	delete(n.Annotations, racer.CacheSizeAnnotationKey)

	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	if err := c.Delete(ctx, site); err != nil {
		t.Fatal(err)
	}

	if got := reconcileNode(n); got.DesiredBytes != racer.DefaultCacheSizeBytes || got.ValidationError != "" {
		t.Fatalf("fallback: %+v", got)
	}
}

func TestStorageDurableBeforeDeliveryAndAmbiguousWrite(t *testing.T) {
	ctx := context.Background()
	n, _, _ := fixtures()
	api := &storageAPI{Client: fakeKube(n)}
	r := newStorageTest(t, api, &Server{})

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: n.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	first := r.server.storagePolicies[identity("node", string(n.UID))]
	if err := api.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
		t.Fatal(err)
	}

	n.Annotations = map[string]string{racer.CacheSizeAnnotationKey: "30Gi"}
	if err := api.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	for _, ambiguous := range []bool{false, true} {
		api.fail, api.ambiguous = !ambiguous, ambiguous

		if _, err := r.Reconcile(ctx, req); err == nil {
			t.Fatal("expected persistence failure")
		}

		if r.server.storagePolicies[first.Node] != first {
			t.Fatal("uncommitted intent published")
		}
	}

	api.fail, api.ambiguous = false, false
	writes := api.writes

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	got := r.server.storagePolicies[first.Node]
	if got.Version != 2 || got.Identity != first.Identity || got.DesiredBytes != 30<<30 || api.writes != writes {
		t.Fatalf("ambiguous recovery: %+v", got)
	}
}

func TestStorageHeartbeatCapabilityAndBoundReports(t *testing.T) {
	f := newCoordinationFixture(t, nil)

	r := newStorageTest(t, f.api, f.s)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}}); err != nil {
		t.Fatal(err)
	}

	first := f.call(t, 0, 200)
	if first.StoragePolicy != nil {
		t.Fatal("old profile-1 client received unsupported policy")
	}

	key := recipient{[32]byte(identityBytes("universe", "default")), [32]byte(identityBytes("node", "node-uid"))}
	if f.s.storageReports[key].State != "unsupported" {
		t.Fatal("missing unsupported observation")
	}

	call := func(version string, boot string) *pb.ControlCommand {
		t.Helper()

		req := httptest.NewRequest("GET", "/", nil)
		req.SetPathValue("universe", identity("universe", "default"))
		req.SetPathValue("node", f.node)
		controlTLS(req, "pod-uid")
		req.Header.Set("X-Racer-Boot", boot)
		req.Header.Set("X-Racer-Profile", "1")
		req.Header.Set("X-Racer-Digest", f.digest)
		req.Header.Set("X-Racer-Phase", "4")
		req.Header.Set("X-Racer-Storage-Policy", "1")
		req.Header.Set("X-Racer-Storage-Identity", f.s.storagePolicies[f.node].Identity)
		req.Header.Set("X-Racer-Storage-Version", version)
		req.Header.Set("X-Racer-Storage-State", "applied")
		req.Header.Set("X-Racer-Storage-Applied-Bytes", strconv.FormatInt(racer.DefaultCacheSizeBytes, 10))

		w := httptest.NewRecorder()
		f.s.control(w, req)

		if w.Code != 200 {
			t.Fatalf("heartbeat: %d %s", w.Code, w.Body.String())
		}

		var command pb.ControlCommand

		if err := proto.Unmarshal(w.Body.Bytes(), &command); err != nil {
			t.Fatal(err)
		}

		return &command
	}
	boot := strings.Repeat("ab", 32)

	delivered := call("1", boot)
	if delivered.Configuration != nil || delivered.StoragePolicy == nil || delivered.Profile != 1 {
		t.Fatal("config-free policy delivery missing")
	}

	if f.s.storageReports[key].State != "pending" {
		t.Fatal("accepted ack before first offer")
	}

	call("1", boot)

	if f.s.storageReports[key].State != "applied" {
		t.Fatal("valid report not accepted")
	}

	for _, phase := range []uint32{1, 2, 3, 4} {
		f.call(t, phase, 200)
	}
	// The legacy fixture call has no storage capability; offer and ack again.
	call("1", boot)
	call("1", boot)

	api := &storageAPI{Client: f.s.controlStore.client}
	f.s.controlStore.client = api
	saved := f.s.storageReports[key]

	for _, stale := range []string{"0", "2", "invalid"} {
		call(stale, boot)

		if f.s.storageReports[key] != saved {
			t.Fatal("stale report changed applied state")
		}
	}
	// Controller restart loses offer/ack, retains durable policy identity/version.
	if api.writes != 0 {
		t.Fatalf("steady heartbeats wrote durable state: writes=%d rollout=%+v", api.writes, f.s.rollouts["default"])
	}

	f.s.storageReports = nil

	restarted := call("1", boot)
	if !proto.Equal(restarted.StoragePolicy, delivered.StoragePolicy) || f.s.storageReports[key].State != "pending" {
		t.Fatal("restart trusted stale acknowledgment")
	}

	call("1", boot)
	// Expire existing topology boot registration before admitting a new process.
	f.s.rollouts["default"].acks = map[string]rolloutAck{}

	call("1", strings.Repeat("cd", 32))

	if f.s.storageReports[key].AppliedBytes != 0 || f.s.storageReports[key].State != "pending" {
		t.Fatal("new process inherited applied state")
	}

	if hex.EncodeToString(delivered.StoragePolicy.Identity) != r.server.storagePolicies[f.node].Identity {
		t.Fatal("wire policy identity mismatch")
	}
	// The durable policy was not folded into the topology generation.
	if !reflect.DeepEqual(f.index.g, f.s.source.topologies[key.universe].g) {
		t.Fatal("heartbeat changed topology")
	}
}

func TestStorageNodeWatchAndInitialInvalidPolicy(t *testing.T) {
	n, _, _ := fixtures()
	old := n.DeepCopy()

	n.Annotations = map[string]string{racer.CacheSizeAnnotationKey: ""}
	if !storageNodeChanged(old, n) || nodeChanged(old, n) {
		t.Fatal("annotation presence must wake only storage reconciliation")
	}

	if !storageNodeChanged(n, old) || storageNodeChanged(n, n.DeepCopy()) {
		t.Fatal("override removal/no-op predicate")
	}

	moved := n.DeepCopy()

	moved.Labels[racer.SiteLabelKey] = "elsewhere"
	if !storageNodeChanged(n, moved) {
		t.Fatal("Site move missed")
	}

	replaced := n.DeepCopy()

	replaced.UID = "new-uid"
	if !storageNodeChanged(n, replaced) {
		t.Fatal("Node replacement missed")
	}

	r := newStorageTest(t, fakeKube(n), &Server{})

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: n.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	got := r.server.storagePolicies[identity("node", string(n.UID))]
	if got.Version != 0 || got.DesiredBytes != 0 || got.ValidationError == "" {
		t.Fatalf("initial invalid policy: %+v", got)
	}
}

func TestStorageHeartbeatRejectsUnauthenticatedReports(t *testing.T) {
	for _, mode := range []string{"plaintext bearer", "unverified certificate", "wrong node", "unselected Pod"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoordinationFixture(t, nil)

			r := newStorageTest(t, f.api, f.s)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}}); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("universe", identity("universe", "default"))
			req.SetPathValue("node", f.node)
			controlTLS(req, "pod-uid")
			req.Header.Set("Authorization", "Bearer pod-token")
			req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
			req.Header.Set("X-Racer-Profile", "1")
			req.Header.Set("X-Racer-Storage-Policy", "1")
			req.Header.Set("X-Racer-Storage-Identity", f.s.storagePolicies[f.node].Identity)
			req.Header.Set("X-Racer-Storage-Version", "1")
			req.Header.Set("X-Racer-Storage-State", "applied")
			req.Header.Set("X-Racer-Storage-Applied-Bytes", strconv.FormatInt(racer.DefaultCacheSizeBytes, 10))

			switch mode {
			case "plaintext bearer":
				req.TLS = nil
			case "unverified certificate":
				req.TLS.VerifiedChains = nil
			case "wrong node":
				req.SetPathValue("node", identity("node", "other-node"))
			case "unselected Pod":
				controlTLS(req, "other-pod")
			}

			w := httptest.NewRecorder()
			f.s.control(w, req)

			if w.Code != http.StatusForbidden || len(f.s.storageReports) != 0 {
				t.Fatalf("unauthorized storage heartbeat: status=%d reports=%+v", w.Code, f.s.storageReports)
			}
		})
	}
}

func TestStorageReportIdentityVersionAndFailureIsolation(t *testing.T) {
	node := identity("node", "node-uid")
	key := recipient{identityBytes("universe", "default"), identityBytes("node", "node-uid")}
	record := storagePolicyRecord{Node: node, Universe: "default", Identity: strings.Repeat("aa", 32), Version: 2, DesiredBytes: 20 << 30}
	s := &Server{storagePolicies: map[string]storagePolicyRecord{node: record}}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
	req.Header.Set("X-Racer-Storage-Policy", "1")
	s.storageCommand(req, key, "pod")
	req.Header.Set("X-Racer-Storage-Identity", record.Identity)
	req.Header.Set("X-Racer-Storage-Version", "2")
	req.Header.Set("X-Racer-Storage-State", "failed")
	req.Header.Set("X-Racer-Storage-Applied-Bytes", strconv.FormatInt(10<<30, 10))
	s.storageCommand(req, key, "pod")

	failed := s.storageReports[key]
	if failed.State != "failed" || failed.AppliedBytes != 10<<30 {
		t.Fatal("failure lost actual bytes")
	}

	for header, value := range map[string]string{
		"X-Racer-Storage-Identity":      strings.Repeat("bb", 32),
		"X-Racer-Storage-Version":       "1",
		"X-Racer-Storage-State":         "applied",
		"X-Racer-Storage-Applied-Bytes": "1",
	} {
		bad := req.Clone(context.Background())
		bad.Header.Set(header, value)
		s.storageCommand(bad, key, "pod")

		if s.storageReports[key] != failed {
			t.Fatalf("bad %s changed report", header)
		}
	}
	// A version change keeps actual bytes but requires a new offer/ack pair.
	record.Version++
	record.DesiredBytes = 30 << 30
	s.storagePolicies[node] = record
	s.storageCommand(req, key, "pod")

	if got := s.storageReports[key]; got.State != "pending" || got.AppliedBytes != 10<<30 {
		t.Fatalf("new version: %+v", got)
	}
	// Pod replacement cannot reuse any former Pod's observation.
	s.storageCommand(req, key, "replacement-pod")

	if got := s.storageReports[key]; got.AppliedBytes != 0 || got.Version != 0 {
		t.Fatalf("Pod inherited report: %+v", got)
	}
	// A moved Node's old-universe draining process must not receive new policy.
	record.Universe = "elsewhere"

	s.storagePolicies[node] = record
	if s.storageCommand(req, key, "replacement-pod") != nil {
		t.Fatal("cross-universe policy delivery")
	}
}
