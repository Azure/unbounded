// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rendezvous

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestReadContactsUsesBoundedGetsAndFullScanFallback(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	holderID := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 8, now, holderID)...)
	manager := testManager(t, client, self, now, func(opts *Options) {
		opts.SlotCount = 8
		opts.ReadsPerRound = 2
		opts.FullScanAfter = 2
	})

	contacts, err := manager.ReadContacts(context.Background(), 0)
	if err != nil {
		t.Fatalf("ReadContacts round 0: %v", err)
	}

	if got := countLeaseActions(client.Actions(), "get"); got != 2 {
		t.Fatalf("round 0 Lease GETs = %d, want 2", got)
	}

	if len(contacts) != 1 {
		t.Fatalf("round 0 contacts = %d, want 1 deduplicated holder", len(contacts))
	}

	before := len(client.Actions())

	if _, err := manager.ReadContacts(context.Background(), 1); err != nil {
		t.Fatalf("ReadContacts round 1: %v", err)
	}

	if got := countLeaseActions(client.Actions()[before:], "get"); got != 8 {
		t.Fatalf("round 1 Lease GETs = %d, want full scan of 8", got)
	}

	assertOnlyLeaseVerbs(t, client.Actions(), "get")
}

func TestClaimAllowsOneHolderAndResumes(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	firstID := testPeerID(t)
	secondID := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	first := testManager(t, client, firstID, now, func(opts *Options) {
		opts.SlotCount = 1
		opts.ReadsPerRound = 1
		opts.ClaimAttemptsPerRound = 1
	})
	second := testManager(t, client, secondID, now, func(opts *Options) {
		opts.SlotCount = 1
		opts.ReadsPerRound = 1
		opts.ClaimAttemptsPerRound = 1
	})

	slot, err := first.Claim(context.Background())
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	if slot == "" {
		t.Fatal("first Claim returned empty slot")
	}

	if secondSlot, err := second.Claim(context.Background()); err != nil || secondSlot != "" {
		t.Fatalf("second Claim = %q, %v; want no slot and nil error", secondSlot, err)
	}

	restarted := testManager(t, client, firstID, now.Add(time.Second), func(opts *Options) {
		opts.SlotCount = 1
		opts.ReadsPerRound = 1
		opts.ClaimAttemptsPerRound = 1
	})
	if resumed, err := restarted.Claim(context.Background()); err != nil || resumed != slot {
		t.Fatalf("restart Claim = %q, %v; want %q", resumed, err, slot)
	}
}

func TestClaimRecordsOccupiedOutcome(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, testPeerID(t))...)
	occupied := 0
	manager := testManager(t, client, testPeerID(t), now, func(opts *Options) {
		singleSlotOptions(opts)
		opts.Metrics.OnSlotClaim = func(outcome string) {
			if outcome == "occupied" {
				occupied++
			}
		}
	})

	if slot, err := manager.Claim(context.Background()); err != nil || slot != "" {
		t.Fatalf("Claim = %q, %v; want no available slot", slot, err)
	}

	if occupied != 1 {
		t.Fatalf("occupied outcomes = %d, want 1", occupied)
	}
}

func TestClaimRejectsEmptyAdvertisement(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	manager := testManager(t, client, testPeerID(t), now, func(opts *Options) {
		singleSlotOptions(opts)
		opts.Addrs = func() []multiaddr.Multiaddr { return nil }
	})

	if slot, err := manager.Claim(context.Background()); err == nil || slot != "" {
		t.Fatalf("Claim = %q, %v; want no slot and address error", slot, err)
	}

	if got := countLeaseActions(client.Actions(), "update"); got != 0 {
		t.Fatalf("Lease updates = %d, want 0 for empty advertisement", got)
	}
}

func TestClaimRejectsOversizedAdvertisement(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	manager := testManager(t, client, testPeerID(t), now, func(opts *Options) {
		singleSlotOptions(opts)
		opts.MaxBundleSize = 16
	})

	if slot, err := manager.Claim(context.Background()); err == nil || slot != "" {
		t.Fatalf("Claim = %q, %v; want no slot and bundle-size error", slot, err)
	}

	if got := countLeaseActions(client.Actions(), "update"); got != 0 {
		t.Fatalf("Lease updates = %d, want 0 for oversized advertisement", got)
	}
}

func TestMetricOutcomeContext(t *testing.T) {
	if got := metricOutcome(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline outcome = %q, want timeout", got)
	}

	if got := metricOutcome(context.Canceled); got != "canceled" {
		t.Fatalf("cancel outcome = %q, want canceled", got)
	}
}

func TestConcurrentClaimConflictCommitsOneHolder(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	resource := coordinationv1.SchemeGroupVersion.WithResource("leases")

	observed, err := client.Tracker().Get(resource, "ns", "gantry-rendezvous-0000")
	if err != nil {
		t.Fatalf("get observed Lease: %v", err)
	}

	client.PrependReactor("get", "leases", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, observed.DeepCopyObject(), nil
	})

	var committed atomic.Bool

	client.PrependReactor("update", "leases", func(action clienttesting.Action) (bool, runtime.Object, error) {
		lease := action.(clienttesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if !committed.CompareAndSwap(false, true) {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"},
				lease.Name,
				errors.New("concurrent claim"),
			)
		}

		if err := client.Tracker().Update(resource, lease, "ns"); err != nil {
			return true, nil, err
		}

		return true, lease, nil
	})

	managers := []*Manager{
		testManager(t, client, testPeerID(t), now, singleSlotOptions),
		testManager(t, client, testPeerID(t), now, singleSlotOptions),
	}

	type result struct {
		slot string
		err  error
	}

	results := make(chan result, len(managers))

	var group sync.WaitGroup
	for _, manager := range managers {
		group.Add(1)
		go func() {
			defer group.Done()

			slot, err := manager.Claim(context.Background())
			results <- result{slot: slot, err: err}
		}()
	}

	group.Wait()
	close(results)

	successes := 0
	conflicts := 0

	for result := range results {
		if result.err == nil && result.slot != "" {
			successes++
		}

		if apierrors.IsConflict(result.err) {
			conflicts++
		}
	}

	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
}

func TestClaimConfirmsAmbiguousCommittedUpdate(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	resource := coordinationv1.SchemeGroupVersion.WithResource("leases")

	client.PrependReactor("update", "leases", func(action clienttesting.Action) (bool, runtime.Object, error) {
		lease := action.(clienttesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if err := client.Tracker().Update(resource, lease, "ns"); err != nil {
			return true, nil, err
		}

		return true, nil, apierrors.NewTimeoutError("response lost after commit", 1)
	})
	manager := testManager(t, client, self, now, singleSlotOptions)

	slot, err := manager.Claim(context.Background())
	if err != nil || slot != "gantry-rendezvous-0000" {
		t.Fatalf("Claim = %q, %v; want confirmed committed slot", slot, err)
	}

	if manager.HeldSlot() != slot {
		t.Fatalf("HeldSlot = %q, want %q", manager.HeldSlot(), slot)
	}

	if got := countLeaseActions(client.Actions(), "update"); got != 1 {
		t.Fatalf("Lease updates = %d, want exactly one", got)
	}
}

func TestEmptySlotColdStartOneTwoAndManyAgents(t *testing.T) {
	for _, agents := range []int{1, 2, 16} {
		t.Run(fmt.Sprintf("agents_%d", agents), func(t *testing.T) {
			now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

			const slots = 8

			client := fake.NewSimpleClientset(testSlots(t, slots, now, "")...)
			claimed := 0

			for range agents {
				manager := testManager(t, client, testPeerID(t), now, func(opts *Options) {
					opts.SlotCount = slots
					opts.ReadsPerRound = 2
					opts.ClaimAttemptsPerRound = slots
				})

				slot, err := manager.Claim(context.Background())
				if err != nil {
					t.Fatalf("Claim: %v", err)
				}

				if slot != "" {
					claimed++
				}
			}

			if want := min(agents, slots); claimed != want {
				t.Fatalf("claimed slots = %d, want %d", claimed, want)
			}
		})
	}
}

func TestExpiredHolderCanBeReplaced(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	oldID := testPeerID(t)
	newID := testPeerID(t)
	lease := testSlots(t, 1, now.Add(-2*time.Minute), oldID)[0].(*coordinationv1.Lease)
	client := fake.NewSimpleClientset(lease)
	manager := testManager(t, client, newID, now, func(opts *Options) {
		opts.SlotCount = 1
		opts.ReadsPerRound = 1
		opts.ClaimAttemptsPerRound = 1
		opts.StaleContactGrace = 0
	})

	slot, err := manager.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	got, err := client.CoordinationV1().Leases("ns").Get(context.Background(), slot, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get claimed Lease: %v", err)
	}

	if holder(got) != newID.String() {
		t.Fatalf("holder = %q, want %q", holder(got), newID)
	}
}

func TestRenewPublishesAddressChange(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	client := fake.NewSimpleClientset(testSlots(t, 1, now, "")...)
	currentAddr := "/ip4/10.0.0.1/tcp/4001"
	manager := testManager(t, client, self, now, func(opts *Options) {
		opts.SlotCount = 1
		opts.ReadsPerRound = 1
		opts.ClaimAttemptsPerRound = 1
		opts.Addrs = func() []multiaddr.Multiaddr { return []multiaddr.Multiaddr{multiaddr.StringCast(currentAddr)} }
	})

	slot, err := manager.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	currentAddr = "/ip4/10.0.0.2/tcp/4001"

	if err := manager.Renew(context.Background()); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	got, err := client.CoordinationV1().Leases("ns").Get(context.Background(), slot, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get renewed Lease: %v", err)
	}

	want := currentAddr + "/p2p/" + self.String()
	if got.Annotations[AnnotationP2PAddrs] != want {
		t.Fatalf("published address = %q, want %q", got.Annotations[AnnotationP2PAddrs], want)
	}
}

func TestReadContactsRejectsMismatchedAndExpiredRecords(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	self := testPeerID(t)
	holderID := testPeerID(t)
	otherID := testPeerID(t)
	leases := testSlots(t, 2, now, holderID)
	leases[0].(*coordinationv1.Lease).Annotations[AnnotationP2PAddrs] = testFullAddr(otherID, "10.0.0.9")
	expired := metav1.NewMicroTime(now.Add(-10 * time.Minute))
	leases[1].(*coordinationv1.Lease).Spec.RenewTime = &expired
	client := fake.NewSimpleClientset(leases...)
	manager := testManager(t, client, self, now, func(opts *Options) {
		opts.SlotCount = 2
		opts.ReadsPerRound = 2
		opts.FullScanAfter = 99
	})

	contacts, err := manager.ReadContacts(context.Background(), 0)
	if err == nil {
		t.Fatal("ReadContacts error = nil, want mismatched peer error")
	}

	if len(contacts) != 0 {
		t.Fatalf("contacts = %v, want none", contacts)
	}
}

func TestReadContactsRejectsOversizedPrimaryRecord(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	leases := testSlots(t, 1, now, testPeerID(t))
	leases[0].(*coordinationv1.Lease).Annotations[AnnotationP2PAddrs] = strings.Repeat("x", defaultMaxBundleSize+1)
	client := fake.NewSimpleClientset(leases...)
	manager := testManager(t, client, testPeerID(t), now, singleSlotOptions)

	if contacts, err := manager.ReadContacts(context.Background(), 0); err == nil || len(contacts) != 0 {
		t.Fatalf("ReadContacts = %v, %v; want no contacts and size error", contacts, err)
	}
}

func TestLeaseFreshnessBoundaries(t *testing.T) {
	renewed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	current := renewed.Add(90 * time.Second)
	client := fake.NewSimpleClientset(testSlots(t, 1, renewed, testPeerID(t))...)
	manager := testManager(t, client, testPeerID(t), current, singleSlotOptions)
	manager.now = func() time.Time { return current }

	lease, err := client.CoordinationV1().Leases("ns").Get(context.Background(), "gantry-rendezvous-0000", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get Lease: %v", err)
	}

	if got := manager.leaseFreshness(lease); got != "fresh" {
		t.Fatalf("freshness at expiry = %q, want fresh", got)
	}

	current = current.Add(time.Nanosecond)

	if got := manager.leaseFreshness(lease); got != "stale" {
		t.Fatalf("freshness after expiry = %q, want stale", got)
	}

	current = renewed.Add(90*time.Second + manager.staleContactGrace)
	if got := manager.leaseFreshness(lease); got != "stale" {
		t.Fatalf("freshness at grace boundary = %q, want stale", got)
	}

	current = current.Add(time.Nanosecond)

	if got := manager.leaseFreshness(lease); got != "expired" {
		t.Fatalf("freshness after grace = %q, want expired", got)
	}
}

func TestExpiredContactIncrementsFreshnessMetric(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	leases := testSlots(t, 1, now.Add(-10*time.Minute), testPeerID(t))
	client := fake.NewSimpleClientset(leases...)
	expired := 0
	manager := testManager(t, client, testPeerID(t), now, func(opts *Options) {
		singleSlotOptions(opts)
		opts.Metrics.OnContact = func(freshness string) {
			if freshness == "expired" {
				expired++
			}
		}
	})

	contacts, err := manager.ReadContacts(context.Background(), 0)
	if err != nil || len(contacts) != 0 {
		t.Fatalf("ReadContacts = %v, %v; want no expired contacts", contacts, err)
	}

	if expired != 1 {
		t.Fatalf("expired freshness metrics = %d, want 1", expired)
	}
}

func TestStaleHolderIncludesReachableSampleContact(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	holderID := testPeerID(t)
	sampledID := testPeerID(t)
	leases := testSlots(t, 1, now.Add(-2*time.Minute), holderID)
	lease := leases[0].(*coordinationv1.Lease)
	lease.Annotations[AnnotationBootstrapSample] = fmt.Sprintf(
		`{"version":1,"peers":[%q]}`,
		testFullAddr(sampledID, "10.0.0.3"),
	)
	client := fake.NewSimpleClientset(leases...)
	manager := testManager(t, client, testPeerID(t), now, singleSlotOptions)

	contacts, err := manager.ReadContacts(context.Background(), 0)
	if err != nil {
		t.Fatalf("ReadContacts: %v", err)
	}

	if len(contacts) != 2 {
		t.Fatalf("contacts = %v, want holder and sampled contact", contacts)
	}

	if contacts[0].Freshness != "stale" || contacts[1].Freshness != "stale" || !contacts[1].Sampled {
		t.Fatalf("contacts = %+v, want stale primary and stale sampled contact", contacts)
	}
}

func singleSlotOptions(opts *Options) {
	opts.SlotCount = 1
	opts.ReadsPerRound = 1
	opts.ClaimAttemptsPerRound = 1
}

func testManager(t *testing.T, client *fake.Clientset, id peer.ID, now time.Time, mutate func(*Options)) *Manager {
	t.Helper()

	opts := Options{
		Leases: client.CoordinationV1().Leases("ns"),
		PeerID: id,
		Addrs: func() []multiaddr.Multiaddr {
			return []multiaddr.Multiaddr{multiaddr.StringCast("/ip4/10.0.0.1/tcp/4001")}
		},
		SlotCount:             4,
		ReadsPerRound:         2,
		ClaimAttemptsPerRound: 2,
		ContactsPerSlot:       4,
		FullScanAfter:         3,
		LeaseDuration:         90 * time.Second,
		StaleContactGrace:     5 * time.Minute,
		Now:                   func() time.Time { return now },
	}
	mutate(&opts)

	manager, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return manager
}

func testSlots(t *testing.T, count int, renew time.Time, holderID peer.ID) []runtime.Object {
	t.Helper()

	result := make([]runtime.Object, count)
	for i := range count {
		name := fmt.Sprintf("gantry-rendezvous-%04d", i)
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}

		if holderID != "" {
			holder := holderID.String()
			duration := int32(90)
			renewTime := metav1.NewMicroTime(renew)
			lease.Spec.HolderIdentity = &holder
			lease.Spec.LeaseDurationSeconds = &duration
			lease.Spec.RenewTime = &renewTime
			lease.Annotations = map[string]string{AnnotationP2PAddrs: testFullAddr(holderID, "10.0.0.2")}
		}

		result[i] = lease
	}

	return result
}

func testPeerID(t *testing.T) peer.ID {
	t.Helper()

	_, publicKey, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	id, err := peer.IDFromPublicKey(publicKey)
	if err != nil {
		t.Fatalf("IDFromPublicKey: %v", err)
	}

	return id
}

func testFullAddr(id peer.ID, ip string) string {
	return "/ip4/" + ip + "/tcp/4001/p2p/" + id.String()
}

func countLeaseActions(actions []clienttesting.Action, verb string) int {
	count := 0

	for _, action := range actions {
		if action.GetResource().Resource == "leases" && action.GetVerb() == verb {
			count++
		}
	}

	return count
}

func assertOnlyLeaseVerbs(t *testing.T, actions []clienttesting.Action, allowed ...string) {
	t.Helper()

	allowedSet := make(map[string]struct{}, len(allowed))
	for _, verb := range allowed {
		allowedSet[verb] = struct{}{}
	}

	for _, action := range actions {
		if action.GetResource().Resource != "leases" {
			continue
		}

		if _, ok := allowedSet[action.GetVerb()]; !ok {
			t.Fatalf("unexpected Lease action %s", action.GetVerb())
		}
	}
}
