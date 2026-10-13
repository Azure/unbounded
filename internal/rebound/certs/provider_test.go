// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	clocktesting "k8s.io/utils/clock/testing"
)

type listener struct{ calls atomic.Int64 }

func (l *listener) Enqueue() { l.calls.Add(1) }

func eventually(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.After(5 * time.Second)

	tick := time.NewTicker(time.Millisecond * 5)
	defer tick.Stop()

	for !condition() {
		select {
		case <-deadline:
			t.Fatal("condition not met")
		case <-tick.C:
		}
	}
}

func TestProviderWatchReadinessAndHotSwap(t *testing.T) {
	r, clock := fixture()
	p := NewProvider(r.Client, "unbounded-system")
	p.clock = clock
	l := &listener{}
	p.AddListener(l)

	if p.Ready() || p.Check(nil) == nil || p.HasSynced() {
		t.Fatal("provider ready before start")
	}

	if p.NeedLeaderElection() || !r.NeedLeaderElection() {
		t.Fatal("wrong leader election")
	}

	if _, ok := any(p).(dynamiccertificates.ControllerRunner); ok {
		t.Fatal("manager must own provider lifecycle")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- p.Start(ctx) }()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("provider failed to stop")
		}
	})

	ready := make(chan error, 1)

	go func() { ready <- p.WaitReady(ctx) }()

	eventually(t, p.HasSynced)

	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: "unbounded-system"}, Data: map[string][]byte{"tls.crt": []byte("broken")}}
	if _, err := r.Client.CoreV1().Secrets("unbounded-system").Create(t.Context(), bad, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	p.load(bad)

	if p.Ready() || l.calls.Load() != 0 {
		t.Fatal("accepted invalid initial content")
	}

	reconcile(t, r)

	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitReady blocked")
	}

	eventually(t, func() bool { return l.calls.Load() == 1 })

	if p.Check(nil) != nil || p.Name() != SecretName {
		t.Fatal("provider not ready")
	}

	cert, key := p.CurrentCertKeyContent()
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		t.Fatal(err)
	}

	cert[0] = 0

	current, _ := p.CurrentCertKeyContent()
	if current[0] == 0 {
		t.Fatal("caller mutated provider content")
	}

	controller := dynamiccertificates.NewDynamicServingCertificateController(&tls.Config{MinVersion: tls.VersionTLS12}, nil, p, nil, nil)
	p.AddListener(controller)

	if err := controller.RunOnce(); err != nil {
		t.Fatal(err)
	}

	controllerDone := make(chan struct{})

	go func() { controller.Run(1, ctx.Done()); close(controllerDone) }()

	t.Cleanup(func() { cancel(); <-controllerDone })

	before, err := controller.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "rebound"})
	if err != nil {
		t.Fatal(err)
	}

	s := getSecret(t, r)
	p.load(s)

	if l.calls.Load() != 1 {
		t.Fatal("notified unchanged content")
	}

	broken := s.DeepCopy()
	broken.Data["tls.key"] = []byte("broken")
	p.load(broken)

	still, _ := p.CurrentCertKeyContent()
	if !bytes.Equal(current, still) || l.calls.Load() != 1 {
		t.Fatal("invalid content replaced good pair")
	}

	leaf, err := parseCertificate(current)
	if err != nil {
		t.Fatal(err)
	}

	clock.SetTime(renewAt(leaf))
	reconcile(t, r)
	eventually(t, func() bool { return l.calls.Load() == 2 })
	eventually(t, func() bool {
		after, getErr := controller.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "rebound"})
		return getErr == nil && !bytes.Equal(before.Certificates[0].Certificate[0], after.Certificates[0].Certificate[0])
	})
}

func TestWaitReadyCancellation(t *testing.T) {
	r, _ := fixture()
	p := NewProvider(r.Client, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := p.WaitReady(ctx); err != context.Canceled {
		t.Fatalf("wanted canceled, got %v", err)
	}
}

func TestProviderCertificateValidity(t *testing.T) {
	r, clock := fixture()
	reconcile(t, r)
	secret := getSecret(t, r)
	leaf, err := parseCertificate(secret.Data[corev1.TLSCertKey])
	require.NoError(t, err)

	for _, tt := range []struct {
		name string
		now  time.Time
	}{
		{name: "not yet valid", now: leaf.NotBefore.Add(-time.Second)},
		{name: "expired", now: leaf.NotAfter},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clock.SetTime(tt.now)

			p := NewProvider(r.Client, "")
			p.clock = clock
			l := &listener{}
			p.AddListener(l)
			p.load(secret)
			require.False(t, p.Ready())
			require.ErrorContains(t, p.Check(nil), "not loaded")
			cert, key := p.CurrentCertKeyContent()
			require.Empty(t, cert)
			require.Empty(t, key)
			require.Zero(t, l.calls.Load())
		})
	}

	clock.SetTime(leaf.NotBefore)

	p := NewProvider(r.Client, "")
	p.clock = clock
	p.load(secret)
	require.NoError(t, p.Check(nil))
	require.NoError(t, p.WaitReady(t.Context()))

	clock.SetTime(leaf.NotAfter.Add(-time.Nanosecond))
	require.True(t, p.Ready())
	clock.SetTime(leaf.NotAfter)
	require.False(t, p.Ready())
	require.ErrorContains(t, p.Check(nil), "not valid now")
	require.Error(t, p.WaitReady(t.Context()))

	reconcile(t, r)
	p.load(getSecret(t, r))
	require.True(t, p.Ready())
	require.NoError(t, p.Check(nil))
}

func TestProviderAcceptsFreshCertificateWithLaggingClock(t *testing.T) {
	r, issuerClock := fixture()
	reconcile(t, r)
	secret := getSecret(t, r)

	p := NewProvider(r.Client, "")
	p.clock = clocktesting.NewFakeClock(issuerClock.Now().Add(-time.Minute))
	p.load(secret)
	require.True(t, p.Ready())
	require.NoError(t, p.WaitReady(t.Context()))
	cert, key := p.CurrentCertKeyContent()
	require.Equal(t, secret.Data[corev1.TLSCertKey], cert)
	require.Equal(t, secret.Data[corev1.TLSPrivateKeyKey], key)

	r.Clock = clocktesting.NewFakeClock(p.clock.Now())
	verify(t, r, secret, 1)
	reconcile(t, r)
	require.Equal(t, secret, getSecret(t, r))
}

func TestProviderRetainsValidPair(t *testing.T) {
	r, clock := fixture()
	reconcile(t, r)
	secret := getSecret(t, r)
	p := NewProvider(r.Client, "")
	p.clock = clock
	l := &listener{}
	p.AddListener(l)
	p.load(secret)

	ca, signer, err := loadCA(secret.Data["ca.crt"], secret.Data["ca.key"], clock.Now())
	require.NoError(t, err)

	for _, offset := range []time.Duration{-2 * time.Hour, time.Hour} {
		cert, key, err := issue(clock.Now().Add(offset), time.Hour, r.dnsNames(), ca, signer)
		require.NoError(t, err)

		bad := secret.DeepCopy()
		bad.Data[corev1.TLSCertKey], bad.Data[corev1.TLSPrivateKeyKey] = cert, key
		p.load(bad)
		currentCert, currentKey := p.CurrentCertKeyContent()
		require.Equal(t, secret.Data[corev1.TLSCertKey], currentCert)
		require.Equal(t, secret.Data[corev1.TLSPrivateKeyKey], currentKey)
		require.EqualValues(t, 1, l.calls.Load())
		require.NoError(t, p.Check(nil))
	}
}
