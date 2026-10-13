// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package certs

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	aggregatorv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregatorfake "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/fake"
	clocktesting "k8s.io/utils/clock/testing"
)

func fixture() (*Rotator, *clocktesting.FakeClock) {
	clock := clocktesting.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	r := &Rotator{Client: fake.NewClientset(), Aggregator: aggregatorfake.NewClientset(&aggregatorv1.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: "v1alpha1.rebound.unbounded-cloud.io"},
		Spec:       aggregatorv1.APIServiceSpec{InsecureSkipTLSVerify: true},
	}), Clock: clock}

	return r, clock
}

func reconcile(t *testing.T, r *Rotator) time.Duration {
	t.Helper()

	delay, err := r.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if delay < time.Second || delay > time.Hour {
		t.Fatalf("invalid requeue %s", delay)
	}

	return delay
}

func getSecret(t *testing.T, r *Rotator) *corev1.Secret {
	t.Helper()

	s, err := r.Client.CoreV1().Secrets(r.Namespace).Get(t.Context(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func putSecret(t *testing.T, r *Rotator, s *corev1.Secret) {
	t.Helper()

	if _, err := r.Client.CoreV1().Secrets(r.Namespace).Update(t.Context(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func verify(t *testing.T, r *Rotator, s *corev1.Secret, count int) *x509.Certificate {
	t.Helper()

	leaf, key, err := loadPair(s.Data["tls.crt"], s.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}

	if key.Curve != elliptic.P256() || leaf.IsCA {
		t.Fatal("invalid leaf")
	}

	if !reflect.DeepEqual(leaf.DNSNames, r.dnsNames()) {
		t.Fatalf("SANs: %v", leaf.DNSNames)
	}

	if !reflect.DeepEqual(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Fatalf("EKUs: %v", leaf.ExtKeyUsage)
	}

	_, rest := pem.Decode(s.Data["tls.crt"])
	if len(rest) != 0 {
		t.Fatal("tls.crt must contain only the leaf")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.Data["ca-bundle.crt"]) {
		t.Fatal("invalid bundle")
	}

	for _, name := range r.dnsNames() {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, CurrentTime: r.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	remaining, got := s.Data["ca-bundle.crt"], 0
	for len(remaining) > 0 {
		block, next := pem.Decode(remaining)
		if block == nil {
			t.Fatal("invalid PEM bundle")
		}

		got++
		remaining = next
	}

	if got != count {
		t.Fatalf("bundle has %d CAs, want %d", got, count)
	}

	service, err := r.Aggregator.ApiregistrationV1().APIServices().Get(t.Context(), r.APIServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if service.Spec.InsecureSkipTLSVerify || !bytes.Equal(service.Spec.CABundle, s.Data["ca-bundle.crt"]) {
		t.Fatal("APIService trust mismatch")
	}

	return leaf
}

func TestIssueIdempotencyAndLeafRenewal(t *testing.T) {
	r, clock := fixture()
	issuedAt := clock.Now()

	reconcile(t, r)
	s := getSecret(t, r)

	leaf := verify(t, r, s, 1)
	if s.Type != corev1.SecretTypeTLS {
		t.Fatal("expected TLS Secret")
	}

	ca, key, err := loadCA(s.Data["ca.crt"], s.Data["ca.key"], clock.Now())
	if err != nil || key.Curve != elliptic.P256() {
		t.Fatalf("invalid CA: %v", err)
	}

	if ca.NotAfter.Sub(issuedAt) != 365*24*time.Hour || leaf.NotAfter.Sub(issuedAt) != 30*24*time.Hour {
		t.Fatal("wrong default lifetimes")
	}

	require.Equal(t, issuedAt.Add(-clockSkewAllowance), ca.NotBefore)
	require.Equal(t, issuedAt.Add(-clockSkewAllowance), leaf.NotBefore)
	require.Equal(t, issuedAt.Add(r.CALifetime*2/3), renewAt(ca))
	require.Equal(t, issuedAt.Add(r.LeafLifetime*2/3), renewAt(leaf))
	require.Equal(t, leaf.NotAfter.Format(time.RFC3339Nano), s.Annotations[leafExpiresAt])

	client := r.Client.(*fake.Clientset)
	agg := r.Aggregator.(*aggregatorfake.Clientset)

	client.ClearActions()
	agg.ClearActions()
	reconcile(t, r)

	for _, action := range append(client.Actions(), agg.Actions()...) {
		if action.GetVerb() != "get" {
			t.Fatalf("unexpected write: %s", action.GetVerb())
		}
	}

	clock.SetTime(issuedAt.Add(r.LeafLifetime * 2 / 3).Add(-time.Second))
	reconcile(t, r)

	if !bytes.Equal(s.Data["tls.crt"], getSecret(t, r).Data["tls.crt"]) {
		t.Fatal("renewed early")
	}

	clock.SetTime(issuedAt.Add(r.LeafLifetime * 2 / 3))
	reconcile(t, r)
	renewed := getSecret(t, r)
	verify(t, r, renewed, 1)

	if bytes.Equal(s.Data["tls.crt"], renewed.Data["tls.crt"]) || !bytes.Equal(s.Data["ca.crt"], renewed.Data["ca.crt"]) {
		t.Fatal("leaf renewal changed wrong material")
	}
}

func TestCARotationStages(t *testing.T) {
	r, clock := fixture()
	reconcile(t, r)
	initial := getSecret(t, r)

	ca, err := parseCertificate(initial.Data["ca.crt"])
	if err != nil {
		t.Fatal(err)
	}

	clock.SetTime(renewAt(ca))
	reconcile(t, r)
	staged := getSecret(t, r)

	oldLeaf := verify(t, r, staged, 2)
	if !bytes.Equal(staged.Data["ca.crt"], initial.Data["ca.crt"]) || len(staged.Data["next-ca.key"]) == 0 {
		t.Fatal("CA was not staged")
	}

	clock.Step(r.CAGrace - time.Second)

	if delay := reconcile(t, r); delay != time.Second {
		t.Fatalf("grace requeue = %s", delay)
	}

	if !bytes.Equal(getSecret(t, r).Data["ca.crt"], initial.Data["ca.crt"]) {
		t.Fatal("promoted early")
	}

	clock.Step(time.Second)
	reconcile(t, r)
	promoted := getSecret(t, r)
	verify(t, r, promoted, 2)

	if !bytes.Equal(promoted.Data["ca.crt"], staged.Data["next-ca.crt"]) || !bytes.Equal(promoted.Data["prev-ca.crt"], initial.Data["ca.crt"]) || len(promoted.Data["next-ca.key"]) != 0 {
		t.Fatal("incorrect promotion")
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(promoted.Data["ca-bundle.crt"])

	if _, err := oldLeaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: clock.Now()}); err != nil {
		t.Fatal("old replica lost trust:", err)
	}

	clock.SetTime(oldLeaf.NotAfter.Add(-time.Second))
	reconcile(t, r)
	verify(t, r, getSecret(t, r), 2)
	clock.Step(time.Second)
	reconcile(t, r)
	drained := getSecret(t, r)
	verify(t, r, drained, 1)

	if len(drained.Data["prev-ca.crt"]) != 0 || drained.Annotations[prevDropAt] != "" {
		t.Fatal("previous CA did not drain")
	}
}

func TestMissingAPIServiceAndPublicationGrace(t *testing.T) {
	r, clock := fixture()
	reconcile(t, r)

	ca, err := parseCertificate(getSecret(t, r).Data["ca.crt"])
	if err != nil {
		t.Fatal(err)
	}

	services := r.Aggregator.ApiregistrationV1().APIServices()
	if err := services.Delete(t.Context(), r.APIServiceName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	clock.SetTime(renewAt(ca))

	if delay := reconcile(t, r); delay != retryInterval {
		t.Fatal("missing APIService must retry")
	}

	staged := getSecret(t, r)
	clock.Step(2 * r.CAGrace)
	reconcile(t, r)

	if !bytes.Equal(getSecret(t, r).Data["ca.crt"], staged.Data["ca.crt"]) {
		t.Fatal("promoted without publication")
	}

	if _, err := services.Create(t.Context(), &aggregatorv1.APIService{ObjectMeta: metav1.ObjectMeta{Name: r.APIServiceName}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	reconcile(t, r)
	// Publication has succeeded. The next pass starts a fresh full grace.
	reconcile(t, r)
	clock.Step(r.CAGrace - time.Second)
	reconcile(t, r)

	if !bytes.Equal(getSecret(t, r).Data["ca.crt"], staged.Data["ca.crt"]) {
		t.Fatal("publication grace not respected")
	}

	clock.Step(time.Second)
	reconcile(t, r)
	verify(t, r, getSecret(t, r), 2)
}

func TestCorruptedSecretRecovery(t *testing.T) {
	for _, field := range []string{"tls.crt", "tls.key", "ca.crt", "ca.key", "ca-bundle.crt"} {
		t.Run(field, func(t *testing.T) {
			r, _ := fixture()
			reconcile(t, r)
			s := getSecret(t, r)
			s.Data[field] = []byte("broken")
			putSecret(t, r, s)
			reconcile(t, r)
			verify(t, r, getSecret(t, r), 1)
		})
	}

	t.Run("wrong SANs and signer", func(t *testing.T) {
		r, _ := fixture()
		reconcile(t, r)
		s := getSecret(t, r)
		other, _ := fixture()
		other.ServiceName = "other"
		reconcile(t, other)
		foreign := getSecret(t, other)
		s.Data["tls.crt"], s.Data["tls.key"] = foreign.Data["tls.crt"], foreign.Data["tls.key"]
		putSecret(t, r, s)
		reconcile(t, r)
		verify(t, r, getSecret(t, r), 1)
	})
}

func TestConflictAndAPIError(t *testing.T) {
	for _, target := range []string{"secret", "apiservice"} {
		t.Run(target, func(t *testing.T) {
			r, _ := fixture()
			reconcile(t, r)

			if target == "secret" {
				s := getSecret(t, r)
				s.Data["tls.key"] = nil
				putSecret(t, r, s)
				r.Client.(*fake.Clientset).PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, SecretName, fmt.Errorf("conflict"))
				})
			} else {
				r.Aggregator.(*aggregatorfake.Clientset).PrependReactor("get", "apiservices", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("unavailable")
				})
			}

			if delay, err := r.Reconcile(t.Context()); err == nil || delay != retryInterval {
				t.Fatalf("wanted retryable error, got %s, %v", delay, err)
			}
		})
	}
}

func TestRotatorStartStops(t *testing.T) {
	r, _ := fixture()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Start(ctx) }()

	t.Cleanup(cancel)
	eventually(t, func() bool {
		s, err := r.Client.CoreV1().Secrets("unbounded-system").Get(t.Context(), SecretName, metav1.GetOptions{})
		return err == nil && len(s.Data["tls.crt"]) > 0
	})
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop")
	}
}

func TestInvalidConfig(t *testing.T) {
	r, _ := fixture()

	r.CAGrace = 365 * 24 * time.Hour
	if _, err := r.Reconcile(t.Context()); err == nil {
		t.Fatal("accepted invalid grace")
	}

	if _, err := (&Rotator{}).Reconcile(t.Context()); err == nil {
		t.Fatal("accepted missing clients")
	}
}

func TestTLSKeyPair(t *testing.T) {
	r, _ := fixture()
	reconcile(t, r)

	s := getSecret(t, r)
	if _, err := tls.X509KeyPair(s.Data["tls.crt"], s.Data["tls.key"]); err != nil {
		t.Fatal(err)
	}
}

func TestWrongSANsWithCurrentSigner(t *testing.T) {
	r, clock := fixture()
	reconcile(t, r)
	s := getSecret(t, r)

	ca, signer, err := loadCA(s.Data["ca.crt"], s.Data["ca.key"], clock.Now())
	if err != nil {
		t.Fatal(err)
	}

	cert, key, err := issue(clock.Now(), r.LeafLifetime, []string{"wrong.example"}, ca, signer)
	if err != nil {
		t.Fatal(err)
	}

	s.Data["tls.crt"], s.Data["tls.key"] = cert, key
	putSecret(t, r, s)
	reconcile(t, r)
	verify(t, r, getSecret(t, r), 1)
}

func TestPreviousCAExpiresFirst(t *testing.T) {
	r, clock := fixture()
	reconcile(t, r)
	s := getSecret(t, r)

	cert, _, err := issue(clock.Now(), time.Hour, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	s.Data["prev-ca.crt"] = cert
	s.Annotations[prevDropAt] = clock.Now().Add(24 * time.Hour).Format(time.RFC3339Nano)
	putSecret(t, r, s)
	reconcile(t, r)
	verify(t, r, getSecret(t, r), 2)
	clock.Step(time.Hour)
	reconcile(t, r)
	verify(t, r, getSecret(t, r), 1)
}

func TestPublicationFailureDoesNotWriteLoop(t *testing.T) {
	r, clock := fixture()
	reconcile(t, r)

	ca, err := parseCertificate(getSecret(t, r).Data["ca.crt"])
	if err != nil {
		t.Fatal(err)
	}

	clock.SetTime(renewAt(ca))
	reconcile(t, r)

	if err := r.Aggregator.ApiregistrationV1().APIServices().Delete(t.Context(), r.APIServiceName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	reconcile(t, r)
	client := r.Client.(*fake.Clientset)
	client.ClearActions()
	clock.Step(time.Second)
	reconcile(t, r)

	for _, action := range client.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("publication failure caused %s", action.GetVerb())
		}
	}
}

func TestCorruptedStagedCAAndBundle(t *testing.T) {
	for _, field := range []string{"next-ca.crt", "next-ca.key", "ca-bundle.crt"} {
		t.Run(field, func(t *testing.T) {
			r, clock := fixture()
			reconcile(t, r)

			ca, err := parseCertificate(getSecret(t, r).Data["ca.crt"])
			if err != nil {
				t.Fatal(err)
			}

			clock.SetTime(renewAt(ca))
			reconcile(t, r)
			s := getSecret(t, r)
			original := bytes.Clone(s.Data["ca.crt"])
			s.Data[field] = []byte("broken")
			putSecret(t, r, s)
			clock.Step(r.CAGrace)
			reconcile(t, r)
			repaired := getSecret(t, r)
			verify(t, r, repaired, 2)

			if !bytes.Equal(original, repaired.Data["ca.crt"]) {
				t.Fatal("promoted untrusted CA")
			}

			if _, _, err := loadCA(repaired.Data["next-ca.crt"], repaired.Data["next-ca.key"], clock.Now()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
