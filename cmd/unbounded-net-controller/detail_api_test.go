// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func serveDetailRequest(t *testing.T, mux *http.ServeMux, method, path, body string) (*httptest.ResponseRecorder, statusv1alpha1.NodeDetailResult) {
	t.Helper()

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))

	var result statusv1alpha1.NodeDetailResult
	if recorder.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}

	return recorder, result
}

func TestDetailAPIRequestAndResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		health := &healthState{detailRequests: manager}
		health.isLeader.Store(true)

		mux := http.NewServeMux()
		registerStatusHandlers(mux, health, false, nil, nil, nil)

		path := "/status/node/node/details"
		response, pending := serveDetailRequest(t, mux, http.MethodPost, path, `{}`)

		if response.Code != http.StatusAccepted || pending.State != statusv1alpha1.NodeDetailPending || pending.RequestID == "" {
			t.Fatalf("POST did not return a pending request: %d %s", response.Code, response.Body.String())
		}

		if err := manager.Complete("node", pending.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		response, complete := serveDetailRequest(t, mux, http.MethodGet, path+"?requestId="+pending.RequestID, "")
		if response.Code != http.StatusOK || complete.State != statusv1alpha1.NodeDetailComplete ||
			complete.Details == nil || complete.Details.Status.NodeInfo.Name != "node" ||
			response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET did not return cached details: %d %s", response.Code, response.Body.String())
		}

		response, reused := serveDetailRequest(t, mux, http.MethodPost, path, `{"forceRefresh":false}`)
		if response.Code != http.StatusOK || reused.RequestID != pending.RequestID {
			t.Fatal("POST did not reuse existing details")
		}

		response, refresh := serveDetailRequest(t, mux, http.MethodPost, path, `{"forceRefresh":true}`)
		if response.Code != http.StatusAccepted || refresh.RequestID == pending.RequestID {
			t.Fatal("forced refresh did not create a new request")
		}

		time.Sleep(manager.timeout)
		synctest.Wait()

		response, expired := serveDetailRequest(t, mux, http.MethodGet, path+"?requestId="+refresh.RequestID, "")

		if response.Code != http.StatusGone || expired.State != statusv1alpha1.NodeDetailExpired || expired.Details != nil {
			t.Fatal("expired request was not explicit")
		}

		health.setLeader(false)

		response, stopped := serveDetailRequest(t, mux, http.MethodGet, path+"?requestId="+pending.RequestID, "")

		if response.Code != http.StatusServiceUnavailable || stopped.State != statusv1alpha1.NodeDetailRetryable || stopped.Details != nil {
			t.Fatal("leadership loss did not produce retryable failure")
		}

		assertNodeDetailEntries(t, manager.cache, 0)
	})
}

func TestDetailAPIMethodsAndErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		health := &healthState{detailRequests: manager}
		health.isLeader.Store(true)

		mux := http.NewServeMux()
		registerStatusHandlers(mux, health, false, nil, nil, nil)

		for _, tc := range []struct {
			method string
			query  string
			body   string
			code   int
		}{
			{http.MethodDelete, "", "", http.StatusMethodNotAllowed},
			{http.MethodGet, "", "", http.StatusBadRequest},
			{http.MethodGet, "?requestId=unknown", "", http.StatusServiceUnavailable},
			{http.MethodPost, "", "", http.StatusBadRequest},
			{http.MethodPost, "", "null", http.StatusBadRequest},
			{http.MethodPost, "", "{} {}", http.StatusBadRequest},
			{http.MethodPost, "", `{"forceRefresh":"yes"}`, http.StatusBadRequest},
			{http.MethodPost, "", `{"url":"http://caller-controlled"}`, http.StatusBadRequest},
			{http.MethodPost, "", strings.Repeat(" ", 1<<20) + "{}", http.StatusRequestEntityTooLarge},
		} {
			response, _ := serveDetailRequest(t, mux, tc.method, "/status/node/node/details"+tc.query, tc.body)
			if response.Code != tc.code {
				t.Fatalf("%s %s: got %d, want %d: %s", tc.method, tc.query, response.Code, tc.code, response.Body.String())
			}

			if tc.code == http.StatusMethodNotAllowed && response.Header().Get("Allow") != "GET, POST" {
				t.Fatal("missing Allow header")
			}
		}

		manager.mu.Lock()
		count := len(manager.requests)
		manager.mu.Unlock()

		if count != 0 {
			t.Fatal("invalid API requests created work")
		}
	})
}

func TestDetailAPIAuthorization(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(strconv.FormatBool(allowed), func(t *testing.T) {
			client := k8sfake.NewClientset()
			client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
				if review.Spec.ResourceAttributes.Name != "dashboard" || review.Spec.ResourceAttributes.Verb != "get" {
					t.Error("detail API changed the existing authorization resource")
				}

				return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
			})

			issuer := testTokenIssuer(t)

			viewer, _, err := issuer.IssueViewerToken("viewer", nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}

			health := &healthState{detailRequests: testDetailRequests(t, nodeDetailRequestHooks{})}
			health.isLeader.Store(true)

			proxy, trustedTLS := testNodeTokenFrontProxy(t)
			mux := http.NewServeMux()
			registerStatusHandlers(mux, health, true, proxy, newDashboardAuthorizer(client), issuer)

			for _, token := range []string{"", "invalid", testNodeToken(t, issuer), viewer} {
				request := httptest.NewRequest(http.MethodPost, "/status/node/node/details", strings.NewReader("{}"))
				if token != "" {
					request.Header.Set("Authorization", "Bearer "+token)
				}

				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request)

				want := http.StatusUnauthorized
				if token == viewer && allowed {
					want = http.StatusAccepted
				}

				if response.Code != want {
					t.Fatalf("authorization: got %d, want %d", response.Code, want)
				}
			}

			request := httptest.NewRequest(http.MethodPost, "/status/node/node/details", strings.NewReader("{}"))
			request.TLS = trustedTLS
			request.Header.Set("X-Remote-User", "aggregated-viewer")

			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			if response.Code != http.StatusAccepted {
				t.Fatalf("trusted aggregated request rejected: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func testDetailLifecycle(t *testing.T, port int) (*healthState, cache.SharedIndexInformer, *nodeDetailRequests) {
	t.Helper()

	health := &healthState{
		statusDetailCacheTTL: 10 * time.Second, statusDetailRequestTimeout: 3 * time.Second,
		nodeAgentHealthPort: port,
	}
	health.isLeader.Store(true)

	factory := informers.NewSharedInformerFactory(k8sfake.NewClientset(), 0)
	informer := factory.Core().V1().Nodes().Informer()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "uid"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}},
	}
	if err := informer.GetIndexer().Add(node); err != nil {
		t.Fatal(err)
	}

	manager, err := health.startDetailRequests(t.Context(), informer)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(manager.Close)

	return health, informer, manager
}

func TestDetailAPIHTTPPull(t *testing.T) {
	for _, mode := range []string{"success", "failure", "wrong-node", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			pullSucceeds := mode == "success" || mode == "oversized"

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status/json" || r.Method != http.MethodGet {
					t.Error("incorrect node detail pull endpoint")
				}

				if mode == "failure" {
					http.Error(w, "unreachable", http.StatusServiceUnavailable)

					return
				}

				status := testDetailStatus()
				if mode == "wrong-node" {
					status.NodeInfo.Name = "other"
				}

				if mode == "oversized" {
					status.NodeInfo.K8sLabels = map[string]string{"large": strings.Repeat("x", 1<<20)}
				}

				json.NewEncoder(w).Encode(status)
			}))
			defer server.Close()

			_, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}

			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}

			health, _, manager := testDetailLifecycle(t, port)
			if health.pullEnabled.Load() {
				t.Fatal("test must exercise disabled background pulls")
			}

			mux := http.NewServeMux()
			registerStatusHandlers(mux, health, false, nil, nil, nil)
			_, request := serveDetailRequest(t, mux, http.MethodPost, "/status/node/node/details", "{}")

			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()

			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()

			for {
				response, result := serveDetailRequest(t, mux, http.MethodGet, "/status/node/node/details?requestId="+request.RequestID, "")
				if pullSucceeds && result.State == statusv1alpha1.NodeDetailComplete {
					if response.Code != http.StatusOK || result.Details == nil || result.Details.Status.NodeInfo.Name != "node" {
						t.Fatal("HTTP pull result is incomplete")
					}

					// The status POST body limit does not limit legacy HTTP pull responses.
					if mode == "oversized" && len(result.Details.Status.NodeInfo.K8sLabels["large"]) != 1<<20 {
						t.Fatal("HTTP pull response was truncated to the POST body limit")
					}

					break
				}

				if !pullSucceeds {
					if _, ok := manager.Pending("node"); ok {
						result = manager.Result("node", request.RequestID)

						if result.Details != nil || result.Error == "" {
							t.Fatal("failed pull returned success-shaped details")
						}

						break
					}
				}

				select {
				case <-deadline.C:
					t.Fatalf("HTTP pull did not settle: %+v", result)
				case <-ticker.C:
				}
			}
		})
	}
}

func TestDetailLifecycleInvalidationAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health, informer, manager := testDetailLifecycle(t, 0)

		request := manager.Request("node", false)
		if err := manager.Complete("node", request.RequestID, testDetailStatus()); err != nil {
			t.Fatal(err)
		}

		oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "uid"}}
		newNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "new"}}
		detailNodeEvents(manager).OnUpdate(oldNode, newNode)
		assertNodeDetailEntries(t, manager.cache, 0)

		if _, err := health.startDetailRequests(t.Context(), informer); err == nil {
			t.Fatal("duplicate lifecycle initialized")
		}

		health.setLeader(false)
		synctest.Wait()

		if health.getDetailRequests() != nil {
			t.Fatal("leadership loss retained manager")
		}

		ctx, cancel := context.WithCancel(t.Context())

		health.isLeader.Store(true)

		restarted, err := health.startDetailRequests(ctx, informer)
		if err != nil {
			t.Fatal(err)
		}

		restarted.Request("node", true)
		detailNodeEvents(restarted).OnDelete(cache.DeletedFinalStateUnknown{Obj: oldNode})
		detailNodeEvents(restarted).OnDelete("invalid")
		cancel()
		restarted.Close()
		synctest.Wait()

		if health.getDetailRequests() != nil {
			t.Fatal("context cancellation retained manager")
		}

		assertNodeDetailEntries(t, restarted.cache, 0)
	})
}
