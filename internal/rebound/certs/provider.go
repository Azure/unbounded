// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

var _ dynamiccertificates.CertKeyContentProvider = (*Provider)(nil)

// Provider retains the last valid key pair if the Secret is deleted or damaged.
// It deliberately does not implement ControllerRunner: the manager owns its
// informer lifecycle, not SecureServingInfo. Start it before calling WaitReady.
type Provider struct {
	informer  cache.SharedIndexInformer
	mu        sync.RWMutex
	cert, key []byte
	listeners []dynamiccertificates.Listener
	ready     chan struct{}
}

func NewProvider(client kubernetes.Interface, namespace string) *Provider {
	if namespace == "" {
		namespace = "unbounded-system"
	}

	return &Provider{
		informer: coreinformers.NewFilteredSecretInformer(client, namespace, 0, cache.Indexers{}, selectName(SecretName)),
		ready:    make(chan struct{}),
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
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return
	}

	p.mu.Lock()
	if bytes.Equal(p.cert, cert) && bytes.Equal(p.key, key) {
		p.mu.Unlock()
		return
	}

	first := len(p.cert) == 0
	p.cert, p.key = bytes.Clone(cert), bytes.Clone(key)

	listeners := append([]dynamiccertificates.Listener(nil), p.listeners...)
	if first {
		close(p.ready)
	}
	p.mu.Unlock()

	for _, listener := range listeners {
		listener.Enqueue()
	}
}

func (p *Provider) Start(ctx context.Context) error {
	_, err := p.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    p.load,
		UpdateFunc: func(_, obj any) { p.load(obj) },
	})
	if err != nil {
		return err
	}

	p.informer.Run(ctx.Done())

	return nil
}

func (p *Provider) NeedLeaderElection() bool { return false }

func (p *Provider) HasSynced() bool { return p.informer.HasSynced() }

func (p *Provider) Ready() bool {
	select {
	case <-p.ready:
		return true
	default:
		return false
	}
}

func (p *Provider) WaitReady(ctx context.Context) error {
	select {
	case <-p.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Provider) Check(_ *http.Request) error {
	if !p.Ready() {
		return fmt.Errorf("serving certificate not loaded")
	}

	return nil
}
