// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func detailResultFixture() statusv1alpha1.NodeDetailResult {
	now := time.Now().UTC()

	return statusv1alpha1.NodeDetailResult{
		State: statusv1alpha1.NodeDetailComplete, NodeName: "node-a", RequestID: "request/+ ?",
		Deadline: now.Add(-time.Second),
		Details: &statusv1alpha1.NodeDetailSnapshot{
			NodeName: "node-a", RequestID: "request/+ ?", CollectedAt: now, ReceivedAt: now,
			ExpiresAt: now.Add(time.Minute),
			Status: &statusv1alpha1.NodeStatusResponse{
				Timestamp: now, NodeInfo: statusv1alpha1.NodeInfo{Name: "node-a"},
				Peers: []statusv1alpha1.PeerStatus{{Name: "peer"}},
			},
		},
	}
}

func TestNodeDetailClientCachedAndPending(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, refresh := range []bool{false, true} {
			result := detailResultFixture()
			calls := 0
			client := nodeDetailClient{
				pollInterval: time.Nanosecond,
				request: func(_ context.Context, method, path string, body []byte) ([]byte, error) {
					calls++
					if calls == 1 {
						if method != http.MethodPost || path != "/status/node/node-a/details" {
							t.Fatalf("initial request = %s %s", method, path)
						}

						var args struct {
							Refresh *bool `json:"forceRefresh"`
						}
						if err := json.Unmarshal(body, &args); err != nil || args.Refresh == nil || *args.Refresh != refresh {
							t.Fatalf("refresh body = %s, %v", body, err)
						}

						if pending {
							result.Deadline = time.Now().Add(time.Minute)

							return json.Marshal(statusv1alpha1.NodeDetailResult{
								State: statusv1alpha1.NodeDetailPending, NodeName: result.NodeName,
								RequestID: result.RequestID, Deadline: result.Deadline,
							})
						}
					} else {
						target, err := url.Parse(path)
						if err != nil || method != http.MethodGet || target.Query().Get("requestId") != result.RequestID || body != nil {
							t.Fatalf("poll request = %s %s, %v", method, path, err)
						}
					}

					return json.Marshal(result)
				},
			}

			got, err := client.fetch(context.Background(), "node-a", refresh)
			if err != nil || got.NodeInfo.Name != "node-a" || len(got.Peers) != 1 {
				t.Fatalf("fetch = %+v, %v", got, err)
			}

			wantCalls := 1
			if pending {
				wantCalls = 2
			}

			if calls != wantCalls {
				t.Errorf("made %d calls, want %d", calls, wantCalls)
			}
		}
	}
}

func TestNodeDetailClientFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*statusv1alpha1.NodeDetailResult)
		want   string
	}{
		{"wrong node", func(r *statusv1alpha1.NodeDetailResult) { r.NodeName = "other" }, "identity mismatch"},
		{"wrong snapshot", func(r *statusv1alpha1.NodeDetailResult) { r.Details.NodeName = "other" }, "malformed"},
		{"wrong request", func(r *statusv1alpha1.NodeDetailResult) { r.Details.RequestID = "other" }, "malformed"},
		{"wrong payload", func(r *statusv1alpha1.NodeDetailResult) { r.Details.Status.NodeInfo.Name = "other" }, "malformed"},
		{"missing details", func(r *statusv1alpha1.NodeDetailResult) { r.Details = nil }, "malformed"},
		{"missing payload", func(r *statusv1alpha1.NodeDetailResult) { r.Details.Status = nil }, "malformed"},
		{"missing collection time", func(r *statusv1alpha1.NodeDetailResult) { r.Details.CollectedAt = time.Time{} }, "malformed"},
		{"missing receipt time", func(r *statusv1alpha1.NodeDetailResult) { r.Details.ReceivedAt = time.Time{} }, "malformed"},
		{"missing request", func(r *statusv1alpha1.NodeDetailResult) { r.RequestID = "" }, "malformed"},
		{"fetch failure", func(r *statusv1alpha1.NodeDetailResult) { r.Details.Status.FetchError = "unreachable" }, "collection failed"},
		{"expired snapshot", func(r *statusv1alpha1.NodeDetailResult) {
			r.Details.ReceivedAt = time.Now().Add(-time.Hour)
			r.Details.ExpiresAt = time.Now().Add(-time.Second)
		}, "expired"},
		{"missing pending ID", func(r *statusv1alpha1.NodeDetailResult) {
			r.State, r.RequestID, r.Details = statusv1alpha1.NodeDetailPending, "", nil
		}, "malformed"},
		{"missing pending deadline", func(r *statusv1alpha1.NodeDetailResult) {
			r.State, r.Deadline, r.Details = statusv1alpha1.NodeDetailPending, time.Time{}, nil
		}, "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := detailResultFixture()
			tc.change(&result)

			client := nodeDetailClient{request: func(context.Context, string, string, []byte) ([]byte, error) {
				return json.Marshal(result)
			}}
			if _, err := client.fetch(context.Background(), "node-a", false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fetch error = %v, want %q", err, tc.want)
			}
		})
	}

	for _, state := range []statusv1alpha1.NodeDetailState{
		statusv1alpha1.NodeDetailExpired, statusv1alpha1.NodeDetailUnavailable, statusv1alpha1.NodeDetailRetryable, "unsupported", "",
	} {
		client := nodeDetailClient{request: func(context.Context, string, string, []byte) ([]byte, error) {
			return json.Marshal(statusv1alpha1.NodeDetailResult{NodeName: "node-a", State: state, Error: "controller explanation"})
		}}
		if _, err := client.fetch(context.Background(), "node-a", false); err == nil ||
			!strings.Contains(err.Error(), string(state)) || !strings.Contains(err.Error(), "controller explanation") {
			t.Errorf("state %q error = %v", state, err)
		}
	}
}

func TestNodeDetailClientDeadlineAndCancellation(t *testing.T) {
	result := detailResultFixture()
	result.State, result.Details, result.Deadline = statusv1alpha1.NodeDetailPending, nil, time.Now().Add(-time.Second)
	calls := 0

	client := nodeDetailClient{request: func(context.Context, string, string, []byte) ([]byte, error) {
		calls++
		return json.Marshal(result)
	}}
	if _, err := client.fetch(context.Background(), "node-a", false); !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("expired deadline = %v, calls=%d", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.fetch(ctx, "node-a", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}

	client.pollInterval = time.Nanosecond
	result.Deadline = time.Now().Add(time.Minute)

	client.request = func(context.Context, string, string, []byte) ([]byte, error) {
		result.Deadline = result.Deadline.Add(time.Second)
		return json.Marshal(result)
	}
	if _, err := client.fetch(context.Background(), "node-a", false); err == nil || !strings.Contains(err.Error(), "deadline changed") {
		t.Fatalf("deadline extension accepted: %v", err)
	}

	client.request = func(context.Context, string, string, []byte) ([]byte, error) { return []byte(`not JSON`), nil }
	if _, err := client.fetch(context.Background(), "node-a", false); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed response = %v", err)
	}

	client.request = func(context.Context, string, string, []byte) ([]byte, error) { return nil, errors.New("HTTP 404") }
	if _, err := client.fetch(context.Background(), "node-a", false); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported API = %v", err)
	}

	if _, err := client.fetch(context.Background(), "../other", false); err == nil || !strings.Contains(err.Error(), "invalid node name") {
		t.Fatalf("invalid node = %v", err)
	}
}
