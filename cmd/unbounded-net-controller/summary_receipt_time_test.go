// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"testing/synctest"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestSummaryNotificationsPreserveReceiptTime(t *testing.T) {
	for _, mode := range []string{"summary", "legacy-full", "legacy-delta"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cache := NewNodeStatusCache()
				cache.BindDetails(testDetailRequests(t, nodeDetailRequestHooks{}))

				cluster := NewClusterStatusCache(&healthState{})
				cluster.status = &ClusterStatusResponse{
					Nodes: []*NodeStatusResponse{{NodeInfo: NodeInfo{Name: "node"}}},
				}
				cluster.nodeIndex["node"] = 0
				cache.SetOnOverviewChange(cluster.PatchOverview)

				claimedTime := time.Unix(1, 0)

				var revision uint64

				publish := func() {
					t.Helper()

					var err error

					switch {
					case mode == "summary":
						revision, err = cache.StoreOverview("node", statusv1alpha1.NodeStatusOverview{
							NodeInfo: NodeInfo{Name: "node"}, LastPushTime: &claimedTime,
						}, "ws")
					case revision == 0 || mode == "legacy-full":
						status := retentionFixture(1)
						status.LastPushTime = &claimedTime
						revision, err = cache.StoreFullChecked("node", status, "ws")
					default:
						now := time.Now()

						var resync bool

						revision, resync, err = cache.ApplyParsedDelta("node", revision, parsedDelta{timestamp: &now}, "ws")
						if resync {
							t.Fatal("unexpected resync")
						}
					}

					if err != nil {
						t.Fatal(err)
					}
				}
				assertReceipt := func(want time.Time) *ClusterSummary {
					t.Helper()

					summary := buildClusterSummary(cluster.Get())

					got := summary.NodeSummaries[0].LastPushTime
					if got == nil || !got.Equal(want) {
						t.Fatalf("summary receipt time = %v, want %v", got, want)
					}

					return summary
				}

				publish()

				entry, _ := cache.Get("node")

				first := assertReceipt(entry.ReceivedAt)
				if !entry.Overview.LastPushTime.Equal(claimedTime) {
					t.Fatal("notification changed the stored wire metadata")
				}

				time.Sleep(time.Second)
				cache.UpdateSource("node", "apiserver-ws")
				assertReceipt(entry.ReceivedAt)
				time.Sleep(time.Second)
				publish()

				nextEntry, _ := cache.Get("node")

				next := assertReceipt(nextEntry.ReceivedAt)
				if !first.NodeSummaries[0].LastPushTime.Equal(entry.ReceivedAt) {
					t.Fatal("new notification mutated an earlier snapshot")
				}

				if delta := computeClusterSummaryDelta(first, next); delta == nil || len(delta.NodeSummaries) != 1 {
					t.Fatal("receipt-only update did not reach the summary delta")
				}
			})
		})
	}
}
