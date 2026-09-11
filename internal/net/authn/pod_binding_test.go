// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

type podBindingFixture struct {
	verifier *PodBoundTokenVerifier
	claims   *kubernetesServiceAccountClaims
	key      *ecdsa.PrivateKey
	pod      *corev1.Pod
	sa       *corev1.ServiceAccount
	pods     cache.Indexer
	sas      cache.Indexer
	ready    bool
	now      time.Time
}

type podBindingVerifierFunc func(context.Context, string) (*KubernetesServiceAccountIdentity, error)

func (f podBindingVerifierFunc) Verify(ctx context.Context, token string) (*KubernetesServiceAccountIdentity, error) {
	return f(ctx, token)
}

func TestPodBoundTokenVerifierRejectsChangesDuringVerification(t *testing.T) {
	for _, change := range []string{"request canceled", "caches stopped", "nil identity", "identity with error"} {
		t.Run(change, func(t *testing.T) {
			f := newPodBindingFixture(t)
			f.populate(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			oidc := f.verifier.verifier

			f.verifier.verifier = podBindingVerifierFunc(func(ctx context.Context, token string) (*KubernetesServiceAccountIdentity, error) {
				identity, err := oidc.Verify(ctx, token)
				if err != nil {
					t.Fatalf("signed test token failed: %v", err)
				}

				switch change {
				case "request canceled":
					cancel()
				case "caches stopped":
					f.ready = false
				case "nil identity":
					return nil, nil
				case "identity with error":
					return identity, errors.New("verification failed")
				}

				return identity, nil
			})
			if identity, err := f.verifier.Verify(ctx, f.token(t)); err == nil || identity != nil {
				t.Fatalf("unsafe verifier result accepted: %+v, %v", identity, err)
			}
		})
	}
}

func newPodBindingFixture(t *testing.T) *podBindingFixture {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	f := &podBindingFixture{
		key: key, ready: true, now: time.Now(),
		pods: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		sas:  cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "unbounded-system", Name: "agent", UID: "pod-uid"},
			Spec:       corev1.PodSpec{ServiceAccountName: "unbounded-net-node", NodeName: "node-a"},
		},
		sa: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "unbounded-system", Name: "unbounded-net-node", UID: "sa-uid"}},
	}
	f.claims = &kubernetesServiceAccountClaims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: "https://issuer.example", Subject: "system:serviceaccount:unbounded-system:unbounded-net-node",
		Audience: jwt.ClaimStrings{"api"}, ExpiresAt: jwt.NewNumericDate(f.now.Add(time.Hour)),
	}}
	f.claims.Kubernetes.Namespace = f.sa.Namespace
	f.claims.Kubernetes.ServiceAccount.Name = f.sa.Name
	f.claims.Kubernetes.ServiceAccount.UID = string(f.sa.UID)
	f.claims.Kubernetes.Pod.Name = f.pod.Name
	f.claims.Kubernetes.Pod.UID = string(f.pod.UID)
	f.claims.Kubernetes.Node.Name = f.pod.Spec.NodeName
	oidc := &KubernetesOIDCVerifier{
		issuer: f.claims.Issuer, audience: "api", now: func() time.Time { return f.now },
		loadedAt: f.now,
		keys:     map[string]oidcSigningKey{"key": {key: &key.PublicKey, algorithm: "ES256"}},
	}
	f.verifier = NewPodBoundTokenVerifier(oidc, PodBoundTokenVerifierOptions{
		Namespace: f.sa.Namespace, ServiceAccount: f.sa.Name,
		Pods: corelisters.NewPodLister(f.pods), ServiceAccounts: corelisters.NewServiceAccountLister(f.sas),
		Ready: func() bool { return f.ready },
	})
	f.verifier.now = func() time.Time { return f.now }

	return f
}

func (f *podBindingFixture) token(t *testing.T) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodES256, f.claims)
	token.Header["kid"] = "key"

	signed, err := token.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}

	return signed
}

func (f *podBindingFixture) populate(t *testing.T) {
	t.Helper()

	if f.pod != nil {
		if err := f.pods.Add(f.pod); err != nil {
			t.Fatal(err)
		}
	}

	if f.sa != nil {
		if err := f.sas.Add(f.sa); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPodBoundTokenVerifier(t *testing.T) {
	wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*podBindingFixture)
		allow  bool
	}{
		{"live objects", func(*podBindingFixture) {}, true},
		{"missing Pod", func(f *podBindingFixture) { f.pod = nil }, false},
		{"missing service account", func(f *podBindingFixture) { f.sa = nil }, false},
		{"replaced Pod", func(f *podBindingFixture) { f.pod.UID = "new-pod" }, false},
		{"replaced service account", func(f *podBindingFixture) { f.sa.UID = "new-sa" }, false},
		{"no Pod claims", func(f *podBindingFixture) { f.claims.Kubernetes.Pod.Name = ""; f.claims.Kubernetes.Pod.UID = "" }, false},
		{"missing Pod name", func(f *podBindingFixture) { f.claims.Kubernetes.Pod.Name = "" }, false},
		{"missing Pod UID", func(f *podBindingFixture) { f.claims.Kubernetes.Pod.UID = "" }, false},
		{"missing service account UID", func(f *podBindingFixture) { f.claims.Kubernetes.ServiceAccount.UID = "" }, false},
		{"wrong Pod name", func(f *podBindingFixture) { f.claims.Kubernetes.Pod.Name = "other" }, false},
		{"wrong namespace", func(f *podBindingFixture) {
			f.claims.Kubernetes.Namespace = "other"
			f.claims.Subject = "system:serviceaccount:other:unbounded-net-node"
			f.pod.Namespace = "other"
			f.sa.Namespace = "other"
		}, false},
		{"wrong service account", func(f *podBindingFixture) {
			f.claims.Kubernetes.ServiceAccount.Name = "other"
			f.claims.Subject = "system:serviceaccount:unbounded-system:other"
			f.pod.Spec.ServiceAccountName = "other"
			f.sa.Name = "other"
		}, false},
		{"Pod belongs to different service account", func(f *podBindingFixture) { f.pod.Spec.ServiceAccountName = "other" }, false},
		{"Pod belongs to different node", func(f *podBindingFixture) { f.pod.Spec.NodeName = "node-b" }, false},
		{"unscheduled Pod", func(f *podBindingFixture) { f.pod.Spec.NodeName = "" }, false},
		{"missing node claim", func(f *podBindingFixture) { f.claims.Kubernetes.Node.Name = "" }, false},
		{"subject mismatch", func(f *podBindingFixture) { f.claims.Subject = "system:serviceaccount:other:other" }, false},
		{"unsynced caches", func(f *podBindingFixture) { f.ready = false }, false},
		{"missing readiness guard", func(f *podBindingFixture) { f.verifier.options.Ready = nil }, false},
		{"wrong audience", func(f *podBindingFixture) { f.claims.Audience = jwt.ClaimStrings{"other"} }, false},
		{"wrong issuer", func(f *podBindingFixture) { f.claims.Issuer = "https://other.example" }, false},
		{"expired token", func(f *podBindingFixture) { f.claims.ExpiresAt = jwt.NewNumericDate(f.now.Add(-time.Hour)) }, false},
		{"invalid signature", func(f *podBindingFixture) {
			f.key = wrongKey
		}, false},
		{"not ready Pod without Node object", func(f *podBindingFixture) {
			f.pod.Status.Phase = corev1.PodFailed
			f.pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPodBindingFixture(t)
			tc.mutate(f)
			f.populate(t)

			identity, err := f.verifier.Verify(t.Context(), f.token(t))
			if (err == nil) != tc.allow || (identity != nil) != tc.allow {
				t.Fatalf("identity=%+v error=%v, want allowed=%v", identity, err, tc.allow)
			}
		})
	}
}

func TestPodBoundTokenVerifierDeletionGrace(t *testing.T) {
	for _, object := range []string{"Pod", "service account"} {
		for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
			t.Run(object+"/"+offset.String(), func(t *testing.T) {
				f := newPodBindingFixture(t)

				deletedAt := metav1.NewTime(f.now.Add(-60*time.Second + offset))
				if object == "Pod" {
					f.pod.DeletionTimestamp = &deletedAt
				} else {
					f.sa.DeletionTimestamp = &deletedAt
				}

				f.populate(t)

				identity, err := f.verifier.Verify(t.Context(), f.token(t))
				if (err == nil) != (offset >= 0) || (identity != nil) != (offset >= 0) {
					t.Fatalf("identity=%+v error=%v, deletion offset=%v", identity, err, offset)
				}
			})
		}
	}
}

func TestPodBoundTokenVerifierRechecksCachedKeys(t *testing.T) {
	for _, object := range []string{"Pod", "service account", "stopped caches", "canceled request", "grace elapsed"} {
		t.Run(object, func(t *testing.T) {
			f := newPodBindingFixture(t)
			f.populate(t)

			token := f.token(t)
			if _, err := f.verifier.Verify(t.Context(), token); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			switch object {
			case "Pod":
				if err := f.pods.Delete(f.pod); err != nil {
					t.Fatal(err)
				}
			case "service account":
				if err := f.sas.Delete(f.sa); err != nil {
					t.Fatal(err)
				}
			case "stopped caches":
				f.ready = false
			case "canceled request":
				cancel()
			case "grace elapsed":
				pod := f.pod.DeepCopy()
				deletedAt := metav1.NewTime(f.now)

				pod.DeletionTimestamp = &deletedAt
				if err := f.pods.Update(pod); err != nil {
					t.Fatal(err)
				}

				f.now = f.now.Add(60 * time.Second)
				if _, err := f.verifier.Verify(ctx, token); err != nil {
					t.Fatalf("exact grace boundary rejected: %v", err)
				}

				f.now = f.now.Add(time.Nanosecond)
			}

			if identity, err := f.verifier.Verify(ctx, token); err == nil || identity != nil {
				t.Fatalf("cached-key authentication bypassed revocation: %+v, %v", identity, err)
			}
		})
	}
}
