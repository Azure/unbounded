// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/net/authn"
)

func revokeNodeAuthObject(t *testing.T, caches *nodeAuthInformers, client *k8sfake.Clientset, object, mutation string) {
	t.Helper()

	pod, sa, _ := nodeAuthObjects()
	deletedAt := metav1.NewTime(time.Now().Add(-61 * time.Second))

	if object == "Pod" {
		switch mutation {
		case "missing":
			if err := client.CoreV1().Pods(pod.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		case "replaced":
			pod.UID = "replacement-pod"
			if _, err := client.CoreV1().Pods(pod.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		case "deleting":
			pod.DeletionTimestamp = &deletedAt
			if _, err := client.CoreV1().Pods(pod.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}

		waitNodeAuthCondition(t, func() bool {
			got, err := caches.pods.Lister().Pods(pod.Namespace).Get(pod.Name)
			if mutation == "missing" {
				return err != nil
			}

			return err == nil && got.UID == pod.UID && got.DeletionTimestamp.Equal(pod.DeletionTimestamp)
		})
	} else {
		switch mutation {
		case "missing":
			if err := client.CoreV1().ServiceAccounts(sa.Namespace).Delete(t.Context(), sa.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		case "replaced":
			sa.UID = "replacement-sa"
			if _, err := client.CoreV1().ServiceAccounts(sa.Namespace).Update(t.Context(), sa, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		case "deleting":
			sa.DeletionTimestamp = &deletedAt
			if _, err := client.CoreV1().ServiceAccounts(sa.Namespace).Update(t.Context(), sa, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}

		waitNodeAuthCondition(t, func() bool {
			got, err := caches.serviceAccounts.Lister().ServiceAccounts(sa.Namespace).Get(sa.Name)
			if mutation == "missing" {
				return err != nil
			}

			return err == nil && got.UID == sa.UID && got.DeletionTimestamp.Equal(sa.DeletionTimestamp)
		})
	}
}

func TestPodBoundAuthenticationRevocation(t *testing.T) {
	proxy, clientTLS := testNodeTokenFrontProxy(t)

	const (
		saToken = "verified-service-account-token"
		subject = "system:serviceaccount:unbounded-system:unbounded-net-node"
		payload = `{"mode":"full","type":"node_status_full","nodeName":"node-a","status":{"nodeInfo":{"name":"node-a","siteName":"unchanged"}}}`
	)

	for _, object := range []string{"Pod", "service account"} {
		for _, mutation := range []string{"missing", "replaced", "deleting"} {
			t.Run(object+"/"+mutation, func(t *testing.T) {
				pod, sa, identity := nodeAuthObjects()
				client := k8sfake.NewClientset(pod, sa)
				caches, _ := startNodeAuthTestCaches(t, client)

				verifier, err := caches.wrapOIDCFactory(func(context.Context, string, string) (serviceAccountTokenVerifier, error) {
					return fakeServiceAccountTokenVerifier{identity: identity}, nil
				})(t.Context(), "", "")
				if err != nil {
					t.Fatal(err)
				}

				h := newJSONIdentityHealth()
				h.nodeTokenVerifier = verifier
				issuer := testTokenIssuer(t)
				mux := http.NewServeMux()
				registerTokenEndpoints(mux, h, proxy, issuer, tokenEndpointConfig{
					nodeServiceAccount: h.nodeServiceAccount, verifier: verifier,
				})
				registerPushHandlers(mux, h, proxy, make(chan struct{}, maxConcurrentNodeWS), issuer)

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.TLS = clientTLS
					mux.ServeHTTP(w, r)
				}))
				defer server.Close()

				request := func(path, body, bearer string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					req.TLS = clientTLS
					req.Header.Set("Authorization", "Bearer "+bearer)
					req.Header.Set("X-Remote-User", subject)
					req.Header.Set(nodeIdentityTokenHeader, saToken)
					req.Header.Set("Content-Type", "application/json")

					resp := httptest.NewRecorder()
					mux.ServeHTTP(resp, req)

					return resp
				}
				tokenBody := `{"serviceAccountToken":"` + saToken + `"}`

				var issued tokenNodeResponse

				for _, path := range []string{directTokenNodePath, aggregatedTokenNodePath} {
					resp := request(path, tokenBody, saToken)
					if resp.Code != http.StatusOK {
						t.Fatalf("initial %s exchange: %d %s", path, resp.Code, resp.Body.String())
					}

					if err := json.Unmarshal(resp.Body.Bytes(), &issued); err != nil {
						t.Fatal(err)
					}
				}

				if resp := request(aggregatedNodeStatusPushPath, payload, saToken); resp.Code != http.StatusOK {
					t.Fatalf("initial HTTP push: %d %s", resp.Code, resp.Body.String())
				}

				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()

				headers := http.Header{
					"X-Remote-User": []string{subject}, nodeIdentityTokenHeader: []string{saToken},
				}

				existing, _, err := websocket.Dial(ctx, server.URL+aggregatedNodeStatusWebSocketPath, &websocket.DialOptions{HTTPHeader: headers})
				if err != nil {
					t.Fatal(err)
				}

				defer func() { _ = existing.CloseNow() }()

				assertAck := func() {
					if err := existing.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
						t.Fatal(err)
					}

					_, data, err := existing.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}

					var reply struct {
						Type string `json:"type"`
					}
					if err := json.Unmarshal(data, &reply); err != nil || reply.Type != "node_status_ack" {
						t.Fatalf("existing WebSocket did not acknowledge: %s, %v", data, err)
					}
				}
				assertAck()
				revokeNodeAuthObject(t, caches, client, object, mutation)

				before := h.statusCache.GetAll()

				client.ClearActions()

				for _, path := range []string{directTokenNodePath, aggregatedTokenNodePath} {
					resp := request(path, tokenBody, saToken)
					if resp.Code != http.StatusUnauthorized {
						t.Fatalf("revoked %s exchange: %d %s", path, resp.Code, resp.Body.String())
					}

					var result tokenNodeResponse
					if json.Unmarshal(resp.Body.Bytes(), &result) == nil && result.Token != "" {
						t.Fatal("issued new credentials after revocation")
					}
				}

				if resp := request(aggregatedNodeStatusPushPath, payload, saToken); resp.Code != http.StatusForbidden {
					t.Fatalf("revoked HTTP push: %d %s", resp.Code, resp.Body.String())
				}

				conn, resp, err := websocket.Dial(ctx, server.URL+aggregatedNodeStatusWebSocketPath, &websocket.DialOptions{HTTPHeader: headers})
				if conn != nil {
					_ = conn.CloseNow()
				}

				if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
					t.Fatalf("revoked WebSocket handshake: response=%+v error=%v", resp, err)
				}

				assertJSONIdentityCache(t, h, before, true)

				if actions := client.Actions(); len(actions) != 0 {
					t.Fatalf("authentication made API requests or fell back: %v", actions)
				}

				// This PR intentionally does not revoke credentials or connections
				// that were issued/authenticated before the cache observed deletion.
				claims, err := issuer.Validate(issued.Token)
				if err != nil || claims.Role != authn.RoleNode || claims.NodeName != "node-a" {
					t.Fatalf("existing HMAC credentials were revoked: %+v, %v", claims, err)
				}

				if resp := request("/status/push", payload, issued.Token); resp.Code != http.StatusOK {
					t.Fatalf("existing HMAC upload failed: %d %s", resp.Code, resp.Body.String())
				}

				assertAck()
			})
		}
	}
}
