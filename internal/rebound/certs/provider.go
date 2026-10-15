// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

var _ dynamiccertificates.CertKeyContentProvider = (*Provider)(nil)

// recheckInterval bounds how long a cached Secret rejected for its validity
// window waits for the local clock to catch up when no Secret event arrives.
const recheckInterval = 30 * time.Second

// Provider retains the last valid key pair if the Secret is deleted or damaged.
// It deliberately does not implement ControllerRunner: the manager owns its
// informer lifecycle, not SecureServingInfo. Start it before calling WaitReady.
type Provider struct {
	informer  cache.SharedIndexInformer
	reloadMu  sync.Mutex
	mu        sync.RWMutex
	cert, key []byte
	listeners []dynamiccertificates.Listener
	ready     chan struct{}
	clock     clock.WithTicker
	leaf      *x509.Certificate
	names     []string
}

func NewProvider(client kubernetes.Interface, namespace, serviceName string) *Provider {
	if namespace == "" {
		namespace = "unbounded-system"
	}

	if serviceName == "" {
		serviceName = "rebound"
	}

	base := serviceName + "." + namespace + ".svc"

	return &Provider{
		informer: coreinformers.NewFilteredSecretInformer(client, namespace, 0, cache.Indexers{}, selectName(SecretName)),
		ready:    make(chan struct{}),
		clock:    clock.RealClock{},
		names:    []string{base, base + ".cluster.local"},
	}
}

func (p *Provider) Name() string { return SecretName }

func (p *Provider) CurrentCertKeyContent() ([]byte, []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return bytes.Clone(p.cert), bytes.Clone(p.key)
}

func (p *Provider) AddListener(listener dynamiccertificates.Listener) {
	p.mu.Lock()
	p.listeners = append(p.listeners, listener)
	p.mu.Unlock()
}

func (p *Provider) load(obj any) {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Name != SecretName {
		return
	}

	cert, key := secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]

	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return
	}

	now := p.clock.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		klog.ErrorS(fmt.Errorf("serving certificate is not valid now"), "Rejected rebound serving certificate")
		return
	}

	bundle, ok := secret.Data["ca-bundle.crt"]
	if !ok {
		bundle = secret.Data["ca.crt"]
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		klog.ErrorS(fmt.Errorf("missing or invalid CA bundle"), "Rejected rebound serving certificate")
		return
	}

	for _, name := range p.names {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:       roots,
			DNSName:     name,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			CurrentTime: now,
		}); err != nil {
			klog.ErrorS(err, "Rejected rebound serving certificate", "dnsName", name)
			return
		}
	}

	p.mu.Lock()
	if bytes.Equal(p.cert, cert) && bytes.Equal(p.key, key) {
		p.mu.Unlock()
		return
	}

	first := len(p.cert) == 0
	p.cert, p.key = bytes.Clone(cert), bytes.Clone(key)
	p.leaf = leaf

	listeners := append([]dynamiccertificates.Listener(nil), p.listeners...)
	if first {
		close(p.ready)
	}
	p.mu.Unlock()

	for _, listener := range listeners {
		listener.Enqueue()
	}
}

// reload loads the latest cached Secret. Events and the periodic recheck share
// it, so a stale object never replaces a newer one.
func (p *Provider) reload() {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	for _, obj := range p.informer.GetStore().List() {
		p.load(obj)
	}
}

// Start runs the informer and rechecks the cached Secret periodically, since
// a certificate rejected as not yet valid can become valid without an event.
func (p *Provider) Start(ctx context.Context) error {
	_, err := p.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { p.reload() },
		UpdateFunc: func(_, _ any) { p.reload() },
	})
	if err != nil {
		return err
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		p.informer.Run(ctx.Done())
	}()

	ticker := p.clock.NewTicker(recheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			<-done

			return nil
		case <-ticker.C():
			p.reload()
		}
	}
}

func (p *Provider) NeedLeaderElection() bool { return false }

func (p *Provider) HasSynced() bool { return p.informer.HasSynced() }

func (p *Provider) Ready() bool {
	return p.Check(nil) == nil
}

func (p *Provider) WaitReady(ctx context.Context) error {
	select {
	case <-p.ready:
		return p.Check(nil)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Provider) Check(_ *http.Request) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.leaf == nil {
		return fmt.Errorf("serving certificate not loaded")
	}

	now := p.clock.Now()
	if now.Before(p.leaf.NotBefore) || !now.Before(p.leaf.NotAfter) {
		return fmt.Errorf("serving certificate is not valid now")
	}

	return nil
}
