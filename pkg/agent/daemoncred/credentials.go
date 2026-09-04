// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemoncred

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/connrotation"
)

const (
	DefaultControllerCertificateSignerName = "kubernetes.io/kube-apiserver-client"

	controllerCertificateStorePrefix               = "daemon-controller"
	defaultControllerCertificateWaitTimeout        = 10 * time.Second
	defaultControllerCertificateWaitPoll           = 500 * time.Millisecond
	defaultControllerCertificateExpirationDuration = 365 * 24 * time.Hour
	defaultControllerCertificateReloadPeriod       = 10 * time.Second
)

// ControllerCertificateOptions configures daemon-controller client certificate
// issuance, storage, and REST client rotation.
type ControllerCertificateOptions struct {
	// Name identifies this certificate manager in logs.
	Name string

	// SignerName is the certificates.k8s.io signerName used for daemon-controller CSRs.
	// Defaults to kubernetes.io/kube-apiserver-client when empty.
	SignerName string

	// DaemonGroup is the additional group requested alongside system:nodes.
	// It is required and must not use reserved or privileged group names.
	DaemonGroup string

	// CredentialDir stores the daemon-controller certificate material.
	// It is required.
	CredentialDir string

	// WaitTimeout is how long initial certificate issuance waits before failing.
	// Defaults to 10 seconds when unset.
	WaitTimeout time.Duration

	// WaitPoll is the polling interval used while waiting for initial issuance.
	// Defaults to 500 milliseconds when unset.
	WaitPoll time.Duration

	// ExpirationDuration is the requested lifetime for issued certificates.
	// Defaults to 365 days when unset.
	ExpirationDuration time.Duration

	// ReloadPeriod is how often the REST config provider checks for rotation.
	// Defaults to 10 seconds when unset.
	ReloadPeriod time.Duration
}

func (o *ControllerCertificateOptions) validate() error {
	if o == nil {
		return fmt.Errorf("controller certificate options are required")
	}

	if o.SignerName == "" {
		o.SignerName = DefaultControllerCertificateSignerName
	}

	if o.Name == "" {
		return fmt.Errorf("certificate manager name is required")
	}

	if o.DaemonGroup == "" {
		return fmt.Errorf("daemon group is required")
	}

	if isReservedDaemonGroup(o.DaemonGroup) {
		return fmt.Errorf("daemon group must not use reserved or privileged group name: %s", o.DaemonGroup)
	}

	if o.WaitTimeout == 0 {
		o.WaitTimeout = defaultControllerCertificateWaitTimeout
	}

	if o.WaitPoll == 0 {
		o.WaitPoll = defaultControllerCertificateWaitPoll
	}

	if o.ExpirationDuration == 0 {
		o.ExpirationDuration = defaultControllerCertificateExpirationDuration
	}

	if o.ReloadPeriod == 0 {
		o.ReloadPeriod = defaultControllerCertificateReloadPeriod
	}

	if o.CredentialDir == "" {
		return fmt.Errorf("credential directory is required")
	}

	if o.WaitTimeout < 0 {
		return fmt.Errorf("wait timeout must be non-negative")
	}

	if o.WaitPoll <= 0 {
		return fmt.Errorf("wait poll must be positive")
	}

	if o.ExpirationDuration <= 0 {
		return fmt.Errorf("expiration duration must be positive")
	}

	if o.ReloadPeriod <= 0 {
		return fmt.Errorf("reload period must be positive")
	}

	return nil
}

func isReservedDaemonGroup(group string) bool {
	group = strings.ToLower(strings.TrimSpace(group))
	if strings.HasPrefix(group, "system:") {
		return true
	}

	switch group {
	case "admin", "admins", "cluster-admin", "cluster-admins", "master", "masters", "root":
		return true
	default:
		return false
	}
}

type RESTConfigProvider struct {
	config       *rest.Config
	manager      certificate.Manager
	closeAll     func()
	reloadPeriod time.Duration
	startOnce    sync.Once
}

func NewRESTConfigProvider(
	ctx context.Context,
	base *rest.Config,
	nodeName string,
	opts ControllerCertificateOptions,
) (*RESTConfigProvider, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	provider, err := newRESTConfigProvider(base, nodeName, opts)
	if err != nil {
		return nil, err
	}

	if provider.manager.Current() == nil {
		provider.start()

		if err := wait.PollUntilContextTimeout(ctx, opts.WaitPoll, opts.WaitTimeout, true, func(context.Context) (bool, error) {
			return provider.manager.Current() != nil, nil
		}); err != nil {
			provider.manager.Stop()
			return nil, fmt.Errorf("wait for daemon controller certificate: %w", err)
		}
	}

	return provider, nil
}

func (p *RESTConfigProvider) RESTConfig() *rest.Config {
	return p.config
}

func (p *RESTConfigProvider) Run(ctx context.Context) {
	p.start()
	defer p.manager.Stop()

	var last *tls.Certificate

	wait.UntilWithContext(ctx, func(context.Context) {
		current := p.manager.Current()
		if current == nil || current == last {
			return
		}

		last = current

		p.closeAll()
	}, p.reloadPeriod)
}

func (p *RESTConfigProvider) start() {
	p.startOnce.Do(func() {
		p.manager.Start()
	})
}

func newRESTConfigProvider(base *rest.Config, nodeName string, opts ControllerCertificateOptions) (*RESTConfigProvider, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("node name is required")
	}

	store, err := newCertificateStore(opts)
	if err != nil {
		return nil, err
	}

	lifetime := opts.ExpirationDuration

	manager, err := certificate.NewManager(&certificate.Config{
		ClientsetFn: func(current *tls.Certificate) (kubernetes.Interface, error) {
			return clientsetForCertificate(base, current)
		},
		Template: &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName:   "system:node:" + nodeName,
				Organization: []string{"system:nodes", opts.DaemonGroup},
			},
		},
		SignerName:                   opts.SignerName,
		RequestedCertificateLifetime: &lifetime,
		Usages: []certificatesv1.KeyUsage{
			certificatesv1.UsageDigitalSignature,
			certificatesv1.UsageClientAuth,
		},
		CertificateStore: store,
		Name:             opts.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("create daemon controller certificate manager: %w", err)
	}

	config, closeAll, err := restConfigWithManager(base, manager)
	if err != nil {
		return nil, err
	}

	return &RESTConfigProvider{
		config:       config,
		manager:      manager,
		closeAll:     closeAll,
		reloadPeriod: opts.ReloadPeriod,
	}, nil
}

func newCertificateStore(opts ControllerCertificateOptions) (certificate.FileStore, error) {
	certPath, keyPath, err := credentialPaths(opts)
	if err != nil {
		return nil, err
	}

	store, err := certificate.NewFileStore(
		controllerCertificateStorePrefix,
		opts.CredentialDir,
		opts.CredentialDir,
		certPath,
		keyPath,
	)
	if err != nil {
		return nil, fmt.Errorf("create daemon controller certificate store: %w", err)
	}

	return store, nil
}

func restConfigWithManager(base *rest.Config, manager certificate.Manager) (*rest.Config, func(), error) {
	cfg := rest.CopyConfig(base)
	cfg.BearerToken = ""
	cfg.BearerTokenFile = ""

	tlsConfig, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("configure TLS: %w", err)
	}

	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}

	tlsConfig.Certificates = nil
	tlsConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert := manager.Current()
		if cert == nil {
			return &tls.Certificate{Certificate: nil}, nil
		}

		return cert, nil
	}

	dialer := connrotation.NewDialer((&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
	cfg.Transport = utilnet.SetTransportDefaults(&http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     tlsConfig,
		MaxIdleConnsPerHost: 25,
		DialContext:         dialer.DialContext,
	})

	cfg.CertData = nil
	cfg.KeyData = nil
	cfg.CertFile = ""
	cfg.KeyFile = ""
	cfg.CAData = nil
	cfg.CAFile = ""
	cfg.Insecure = false
	cfg.NextProtos = nil

	return cfg, dialer.CloseAll, nil
}

func clientsetForCertificate(base *rest.Config, current *tls.Certificate) (kubernetes.Interface, error) {
	cfg := rest.CopyConfig(base)
	if current == nil {
		return kubernetes.NewForConfig(cfg)
	}

	certPEM, err := encodeCertificateChain(current.Certificate)
	if err != nil {
		return nil, err
	}

	keyPEM, err := encodePrivateKey(current.PrivateKey)
	if err != nil {
		return nil, err
	}

	cfg.BearerToken = ""
	cfg.BearerTokenFile = ""
	cfg.CertFile = ""
	cfg.KeyFile = ""
	cfg.CertData = certPEM
	cfg.KeyData = keyPEM

	return kubernetes.NewForConfig(cfg)
}

func encodeCertificateChain(chain [][]byte) ([]byte, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("certificate chain is empty")
	}

	out := make([]byte, 0)
	for _, der := range chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}

	return out, nil
}

func encodePrivateKey(key any) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func credentialPaths(opts ControllerCertificateOptions) (string, string, error) {
	return filepath.Join(opts.CredentialDir, "client.crt"), filepath.Join(opts.CredentialDir, "client.key"), nil
}
