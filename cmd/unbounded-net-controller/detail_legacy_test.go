// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/apimachinery/pkg/types"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestObserveLegacyReusesAssociation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})

		status := testDetailStatus()
		if err := manager.ObserveLegacy("node", status, 1, nil, nil); err != nil {
			t.Fatal(err)
		}

		initial := manager.Request("node", false)
		if initial.State != statusv1alpha1.NodeDetailComplete {
			t.Fatal("legacy data was not reusable")
		}

		time.Sleep(time.Second)

		for revision := uint64(2); revision <= 100; revision++ {
			if err := manager.ObserveLegacy("node", status, revision, nil, nil); err != nil {
				t.Fatal(err)
			}
		}

		updated := manager.Result("node", initial.RequestID)
		if updated.State != statusv1alpha1.NodeDetailComplete || updated.Details == nil ||
			updated.Details.ReceivedAt != initial.Details.ReceivedAt.Add(time.Second) {
			t.Fatal("legacy updates replaced the completed association or failed to refresh TTL")
		}

		manager.mu.Lock()
		count := len(manager.requests)
		manager.mu.Unlock()

		if count != 1 {
			t.Fatalf("publications accumulated %d request records", count)
		}

		fresh := manager.Request("node", true)
		if err := manager.ObserveLegacy("node", status, 101, nil, nil); err != nil {
			t.Fatal(err)
		}

		if result := manager.Result("node", fresh.RequestID); result.State != statusv1alpha1.NodeDetailPending {
			t.Fatal("uncorrelated legacy publication fulfilled a fresh request")
		}

		if err := manager.Complete("node", fresh.RequestID, status); err != nil {
			t.Fatal(err)
		}

		if err := manager.ObserveLegacy("node", status, 102, nil, nil); err != nil {
			t.Fatal(err)
		}

		if result := manager.Result("node", fresh.RequestID); result.State != statusv1alpha1.NodeDetailComplete || result.Details == nil {
			t.Fatal("continuous legacy publication immediately expired a requested result")
		}
	})
}

func TestLegacyBaseExpiryReplacementAndIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		uid := types.UID("uid")
		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Resolve: func(string) (types.UID, error) { return uid, nil },
		})

		memo := &peerIdentityDigest{1}
		if err := manager.ObserveLegacy("node", testDetailStatus(), 5, memo, nil); err != nil {
			t.Fatal(err)
		}

		base, identity, ok := manager.LegacyBase("node", 5)
		if !ok || identity != memo {
			t.Fatal("wire base did not retain its validation memo")
		}

		if _, _, ok := manager.LegacyBase("node", 4); ok {
			t.Fatal("wrong wire revision accepted")
		}

		request := manager.Request("node", true)
		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		if _, _, ok := manager.LegacyBase("node", 5); ok {
			t.Fatal("one-shot details masqueraded as legacy wire base")
		}

		if err := manager.ObserveLegacy("node", testDetailStatus(), 6, memo, base); err == nil {
			t.Fatal("a delta overwrote a newer one-shot result")
		}

		if err := manager.ObserveLegacy("node", testDetailStatus(), 7, memo, nil); err != nil {
			t.Fatal(err)
		}

		time.Sleep(manager.cache.ttl)
		synctest.Wait()
		assertNodeDetailEntries(t, manager.cache, 0)

		if _, _, ok := manager.LegacyBase("node", 7); ok {
			t.Fatal("expired wire base accepted")
		}

		if err := manager.ObserveLegacy("node", testDetailStatus(), 8, memo, nil); err != nil {
			t.Fatal(err)
		}

		synctest.Wait()

		uid = "replacement"

		if _, _, ok := manager.LegacyBase("node", 8); ok {
			t.Fatal("replaced node reused old details")
		}

		assertNodeDetailEntries(t, manager.cache, 0)

		uid = ""

		if err := manager.ObserveLegacy("node", testDetailStatus(), 9, nil, nil); err == nil {
			t.Fatal("unknown node UID accepted")
		}

		if err := manager.ObserveLegacy("node", nil, 9, nil, nil); err == nil {
			t.Fatal("nil legacy details accepted")
		}

		manager.Close()

		if err := manager.ObserveLegacy("node", testDetailStatus(), 9, nil, nil); err == nil {
			t.Fatal("closed manager accepted legacy details")
		}
	})
}

func TestCompleteDetailFailurePreservesCachedResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		if err := manager.ObserveLegacy("node", testDetailStatus(), 1, nil, nil); err != nil {
			t.Fatal(err)
		}

		cached := manager.Request("node", false)
		if err := manager.CompleteFailure("node", cached.RequestID, "obsolete failure"); err != nil {
			t.Fatal(err)
		}

		refresh := manager.Request("node", true)

		for _, wrong := range []struct{ node, id, message string }{
			{"other", refresh.RequestID, "failed"},
			{"node", "unknown", "failed"},
			{"node", refresh.RequestID, ""},
		} {
			if err := manager.CompleteFailure(wrong.node, wrong.id, wrong.message); err == nil {
				t.Fatal("uncorrelated or empty failure accepted")
			}
		}

		for range 2 {
			if err := manager.CompleteFailure("node", refresh.RequestID, "collection failed"); err != nil {
				t.Fatal(err)
			}
		}

		result := manager.Result("node", refresh.RequestID)
		if result.State != statusv1alpha1.NodeDetailUnavailable || result.Error != "collection failed" || result.Details != nil {
			t.Fatal("failure was not explicit")
		}

		previous := manager.Result("node", cached.RequestID)
		if previous.Details == nil || *previous.Details != *cached.Details {
			t.Fatal("failed refresh modified valid old details")
		}

		if _, ok := manager.Pending("node"); ok {
			t.Fatal("failed request retained a command")
		}

		request := manager.Request("node", true)
		time.Sleep(manager.timeout)
		synctest.Wait()

		if err := manager.CompleteFailure("node", request.RequestID, "late"); err == nil {
			t.Fatal("late failure revived an expired request")
		}

		manager.Forget("node")
		assertNodeDetailEntries(t, manager.cache, 0)
	})
}

func TestObserveLegacyReplacementCancelsOldPendingRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		uid := types.UID("old")
		manager := testDetailRequests(t, nodeDetailRequestHooks{
			Resolve: func(string) (types.UID, error) { return uid, nil },
		})
		request := manager.Request("node", true)

		synctest.Wait()

		uid = "new"

		if err := manager.ObserveLegacy("node", testDetailStatus(), 1, nil, nil); err != nil {
			t.Fatal(err)
		}

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailUnavailable {
			t.Fatal("replacement retained the old pending request")
		}

		if _, ok := manager.Pending("node"); ok {
			t.Fatal("replacement retained an old polling command")
		}
	})
}
