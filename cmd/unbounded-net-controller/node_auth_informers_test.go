// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/Azure/unbounded/internal/net/authn"
)

func nodeAuthObjects() (*corev1.Pod, *corev1.ServiceAccount, *authn.KubernetesServiceAccountIdentity) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "unbounded-system", Name: "agent", UID: "pod-uid",
			Labels: map[string]string{"app.kubernetes.io/name": "unbounded-net-node"},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "unbounded-net-node", NodeName: "node-a"},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Spec.ServiceAccountName, UID: "sa-uid"}}
	identity := &authn.KubernetesServiceAccountIdentity{
		Subject:   "system:serviceaccount:unbounded-system:unbounded-net-node",
		Namespace: pod.Namespace, ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID),
		PodName: pod.Name, PodUID: string(pod.UID), NodeName: pod.Spec.NodeName,
	}

	return pod, sa, identity
}

func waitNodeAuthCondition(t *testing.T, condition func() bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for authentication cache state")
		case <-ticker.C:
		}
	}
}

func startNodeAuthTestCaches(t *testing.T, client *k8sfake.Clientset) (*nodeAuthInformers, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	caches := newNodeAuthInformers(ctx, client, "unbounded-system", 0)
	caches.start()
	t.Cleanup(func() {
		cancel()
		caches.podFactory.Shutdown()
		caches.saFactory.Shutdown()
	})
	waitNodeAuthCondition(t, caches.ready)

	return caches, cancel
}

func TestNodeAuthInformersScopeAndLifecycle(t *testing.T) {
	pod, sa, _ := nodeAuthObjects()
	unlabeled := pod.DeepCopy()
	unlabeled.Name = "unlabeled"
	unlabeled.Labels = nil
	otherPod := pod.DeepCopy()
	otherPod.Namespace = "other"
	otherSA := sa.DeepCopy()
	otherSA.Namespace = "other"
	client := k8sfake.NewClientset(pod, sa, unlabeled, otherPod, otherSA)
	caches, stop := startNodeAuthTestCaches(t, client)

	if _, err := caches.pods.Lister().Pods(pod.Namespace).Get(pod.Name); err != nil {
		t.Fatal(err)
	}

	if _, err := caches.serviceAccounts.Lister().ServiceAccounts(sa.Namespace).Get(sa.Name); err != nil {
		t.Fatalf("unlabeled service account must be watched: %v", err)
	}

	for _, excluded := range []*corev1.Pod{unlabeled, otherPod} {
		if _, err := caches.pods.Lister().Pods(excluded.Namespace).Get(excluded.Name); err == nil {
			t.Fatalf("cached excluded Pod %s/%s", excluded.Namespace, excluded.Name)
		}
	}

	if _, err := caches.serviceAccounts.Lister().ServiceAccounts(otherSA.Namespace).Get(otherSA.Name); err == nil {
		t.Fatal("cached service account outside controller namespace")
	}

	for _, action := range client.Actions() {
		if action.GetNamespace() != "unbounded-system" {
			t.Fatalf("unexpected cluster-wide cache request: %v", action)
		}

		if action.GetVerb() != "list" && action.GetVerb() != "watch" {
			t.Fatalf("unexpected informer API request: %v", action)
		}

		selector := ""

		switch action := action.(type) {
		case k8stesting.ListAction:
			selector = action.GetListRestrictions().Labels.String()
		case k8stesting.WatchAction:
			selector = action.GetWatchRestrictions().Labels.String()
		}

		wantSelector := ""
		if action.GetResource().Resource == "pods" {
			wantSelector = "app.kubernetes.io/name=unbounded-net-node"
		}

		if selector != wantSelector {
			t.Fatalf("unexpected %s selector %q, want %q", action.GetResource().Resource, selector, wantSelector)
		}
	}

	leaderCtx, stopLeader := context.WithCancel(t.Context())
	stopLeader()

	if !cache.WaitForCacheSync(leaderCtx.Done(), caches.pods.Informer().HasSynced) && !caches.pods.Informer().HasSynced() {
		t.Fatal("shared Pod informer lost its initial sync")
	}

	if !caches.ready() {
		t.Fatal("leader cancellation stopped process authentication caches")
	}

	if err := client.CoreV1().Pods(pod.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	waitNodeAuthCondition(t, func() bool {
		_, err := caches.pods.Lister().Pods(pod.Namespace).Get(pod.Name)
		return err != nil
	})

	stop()

	if caches.ready() {
		t.Fatal("process cancellation left authentication available")
	}

	caches.podFactory.Shutdown()
	caches.saFactory.Shutdown()

	if !caches.pods.Informer().IsStopped() || !caches.serviceAccounts.Informer().IsStopped() || caches.ready() {
		t.Fatal("stopped informers left authentication available")
	}
}

func TestNodeAuthInformersInitialSyncFailure(t *testing.T) {
	for _, resource := range []string{"pods", "serviceaccounts"} {
		t.Run(resource, func(t *testing.T) {
			pod, sa, identity := nodeAuthObjects()
			client := k8sfake.NewClientset(pod, sa)
			failed := make(chan struct{}, 1)

			client.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				select {
				case failed <- struct{}{}:
				default:
				}

				return true, nil, errors.New("forbidden")
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			caches := newNodeAuthInformers(ctx, client, pod.Namespace, 0)
			caches.start()

			defer func() {
				cancel()
				caches.podFactory.Shutdown()
				caches.saFactory.Shutdown()
			}()

			select {
			case <-failed:
			case <-time.After(5 * time.Second):
				t.Fatal("informer did not attempt initial list")
			}

			verifier, err := caches.wrapOIDCFactory(func(context.Context, string, string) (serviceAccountTokenVerifier, error) {
				return fakeServiceAccountTokenVerifier{identity: identity}, nil
			})(ctx, "", "")
			if err != nil {
				t.Fatal(err)
			}

			if got, err := verifier.Verify(t.Context(), "token"); got != nil || err == nil {
				t.Fatalf("initial list failure bypassed: %+v, %v", got, err)
			}
		})
	}
}

func TestNodeAuthInformersStoppedCache(t *testing.T) {
	for _, resource := range []string{"pods", "serviceaccounts"} {
		t.Run(resource, func(t *testing.T) {
			pod, sa, identity := nodeAuthObjects()
			client := k8sfake.NewClientset(pod, sa)
			caches := newNodeAuthInformers(t.Context(), client, pod.Namespace, 0)
			podCtx, stopPods := context.WithCancel(t.Context())
			saCtx, stopSAs := context.WithCancel(t.Context())

			caches.podFactory.Start(podCtx.Done())
			caches.saFactory.Start(saCtx.Done())

			defer func() {
				stopPods()
				stopSAs()
				caches.podFactory.Shutdown()
				caches.saFactory.Shutdown()
			}()

			waitNodeAuthCondition(t, caches.ready)

			verifier, err := caches.wrapOIDCFactory(func(context.Context, string, string) (serviceAccountTokenVerifier, error) {
				return fakeServiceAccountTokenVerifier{identity: identity}, nil
			})(t.Context(), "", "")
			if err != nil {
				t.Fatal(err)
			}

			if _, err := verifier.Verify(t.Context(), "token"); err != nil {
				t.Fatal(err)
			}

			if resource == "pods" {
				stopPods()
				waitNodeAuthCondition(t, caches.pods.Informer().IsStopped)
			} else {
				stopSAs()
				waitNodeAuthCondition(t, caches.serviceAccounts.Informer().IsStopped)
			}

			if !caches.pods.Informer().HasSynced() || !caches.serviceAccounts.Informer().HasSynced() || caches.ctx.Err() != nil {
				t.Fatal("test requires initially synced caches and a live process context")
			}

			if identity, err := verifier.Verify(t.Context(), "token"); err == nil || identity != nil {
				t.Fatalf("stopped cache accepted identity: %+v, %v", identity, err)
			}
		})
	}
}

func TestInitializedOIDCVerifierChecksBoundObjects(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "discovered"
		if explicit {
			name = "explicit"
		}

		t.Run(name, func(t *testing.T) {
			pod, sa, identity := nodeAuthObjects()
			client := k8sfake.NewClientset(pod, sa)
			caches, _ := startNodeAuthTestCaches(t, client)
			tokenPath := filepath.Join(t.TempDir(), "token")

			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://issuer.example","aud":["api"]}`)) + ".signature"
			if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}

			issuer := ""
			if explicit {
				issuer = "https://issuer.example"
			}

			verifier, err := initializeNodeTokenVerifier(t.Context(), client, issuer, "", tokenPath,
				caches.wrapOIDCFactory(func(context.Context, string, string) (serviceAccountTokenVerifier, error) {
					return fakeServiceAccountTokenVerifier{identity: identity}, nil
				}))
			if err != nil {
				t.Fatal(err)
			}

			if got, err := verifier.Verify(t.Context(), "token"); err != nil || got == nil {
				t.Fatalf("live Pod authentication failed: %+v, %v", got, err)
			}

			if err := client.CoreV1().Pods(pod.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}

			waitNodeAuthCondition(t, func() bool {
				_, err := caches.pods.Lister().Pods(pod.Namespace).Get(pod.Name)
				return err != nil
			})

			if got, err := verifier.Verify(t.Context(), "token"); err == nil || got != nil {
				t.Fatalf("revoked Pod authenticated: %+v, %v", got, err)
			}

			for _, action := range client.Actions() {
				if action.GetResource().Resource == "tokenreviews" {
					t.Fatal("cache miss dynamically fell back to TokenReview")
				}
			}
		})
	}
}
