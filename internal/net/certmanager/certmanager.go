// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package certmanager manages TLS serving certificates for the controller
// using a self-signed CA. It generates a CA key pair, uses it to sign server
// certificates, persists both in a Kubernetes Secret, and publishes the CA
// certificate in a ConfigMap so node agents can verify the controller.
package certmanager

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded-kube/internal/net/authn"
)

const (
	defaultSecretName    = "unbounded-net-serving-cert"
	defaultCAConfigMap   = "unbounded-net-serving-ca"
	rotationThreshold    = 30 * 24 * time.Hour  // rotate server cert when within 30 days of expiry
	caRotationThreshold  = 365 * 24 * time.Hour // rotate CA when within 1 year of expiry
	monitorInterval      = 24 * time.Hour
	caValidityDuration   = 10 * 365 * 24 * time.Hour // 10 years
	certValidityDuration = 365 * 24 * time.Hour      // 1 year
)

// Options configures the CertManager.
type Options struct {
	Clientset   kubernetes.Interface
	Namespace   string // e.g. "kube-system"
	ServiceName string // e.g. "unbounded-net-controller"
	SecretName  string // defaults to "unbounded-net-serving-cert"
	CAConfigMap string // defaults to "unbounded-net-serving-ca"
}

// CertManager manages a self-signed CA and uses it to issue serving
// certificates for the controller. The CA and server key material are
// persisted in a Kubernetes Secret so they survive restarts. The CA
// public certificate is published in a ConfigMap for node agents.
// When the server certificate is within 30 days of expiry it is
// re-issued using the same CA. An HMAC signing key is also stored in
// the Secret for token issuance; the key is generated once and preserved
// across certificate rotations.
type CertManager struct {
	clientset   kubernetes.Interface
	namespace   string
	serviceName string
	secretName  string
	caConfigMap string
	certValue   atomic.Value // holds *tls.Certificate
	caBundle    atomic.Value // holds []byte (PEM-encoded CA cert)
	hmacKey     atomic.Value // holds []byte (HMAC signing key)
}

// NewCertManager creates a new CertManager with the given options, applying
// defaults where needed.
func NewCertManager(opts Options) *CertManager {
	secretName := opts.SecretName
	if secretName == "" {
		secretName = defaultSecretName
	}

	caConfigMap := opts.CAConfigMap
	if caConfigMap == "" {
		caConfigMap = defaultCAConfigMap
	}

	return &CertManager{
		clientset:   opts.Clientset,
		namespace:   opts.Namespace,
		serviceName: opts.ServiceName,
		secretName:  secretName,
		caConfigMap: caConfigMap,
	}
}

// EnsureCertificate loads the serving certificate from the Kubernetes Secret,
// validates it, and rotates it if missing, expired, or expiring soon. It also
// ensures the CA certificate is published to the ConfigMap.
func (cm *CertManager) EnsureCertificate(ctx context.Context) error {
	secret, err := cm.clientset.CoreV1().Secrets(cm.namespace).Get(ctx, cm.secretName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get secret %s/%s: %w", cm.namespace, cm.secretName, err)
		}

		klog.Infof("Secret %s/%s not found, rotating certificate", cm.namespace, cm.secretName)

		return cm.rotateCertificate(ctx)
	}

	certPEM := secret.Data["tls.crt"]
	keyPEM := secret.Data["tls.key"]
	caCertPEM := secret.Data["ca.crt"]
	caKeyPEM := secret.Data["ca.key"]

	if len(certPEM) == 0 || len(keyPEM) == 0 {
		klog.Infof("Secret %s/%s missing tls.crt or tls.key, rotating certificate", cm.namespace, cm.secretName)
		return cm.rotateCertificate(ctx)
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		klog.Warningf("Failed to parse certificate from secret: %v, rotating", err)
		return cm.rotateCertificate(ctx)
	}

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		klog.Warningf("Failed to parse x509 certificate: %v, rotating", err)
		return cm.rotateCertificate(ctx)
	}

	if !cm.validateCertificate(leaf) {
		klog.Infof("Server certificate validation failed, rotating")
		return cm.rotateCertificate(ctx)
	}

	// Validate the CA cert is present and not expired.
	if len(caCertPEM) == 0 || len(caKeyPEM) == 0 {
		klog.Infof("Secret %s/%s missing ca.crt or ca.key, rotating certificate", cm.namespace, cm.secretName)
		return cm.rotateCertificate(ctx)
	}

	caCert, err := parseCertPEM(caCertPEM)
	if err != nil {
		klog.Warningf("Failed to parse CA certificate: %v, rotating", err)
		return cm.rotateCertificate(ctx)
	}

	if time.Until(caCert.NotAfter) <= caRotationThreshold {
		klog.Infof("CA certificate expires within rotation threshold, rotating")
		return cm.rotateCertificate(ctx)
	}

	cm.certValue.Store(&tlsCert)
	cm.caBundle.Store(append([]byte(nil), caCertPEM...))

	// Load HMAC signing key from the Secret.
	hmacKeyData := secret.Data["hmac.key"]
	if len(hmacKeyData) >= authn.HMACKeySize {
		cm.hmacKey.Store(append([]byte(nil), hmacKeyData...))
		klog.Infof("Loaded HMAC signing key from secret %s/%s", cm.namespace, cm.secretName)
	} else {
		klog.Warningf("Secret %s/%s missing or short hmac.key, will generate on next rotation", cm.namespace, cm.secretName)
		// Force rotation so the HMAC key gets generated and stored.
		return cm.rotateCertificate(ctx)
	}

	klog.Infof("Loaded existing serving certificate from secret %s/%s (expires %s)",
		cm.namespace, cm.secretName, leaf.NotAfter.Format(time.RFC3339))

	// Ensure the CA ConfigMap is published (in case it was deleted).
	if err := cm.publishCA(ctx, caCertPEM); err != nil {
		return fmt.Errorf("failed to publish CA ConfigMap: %w", err)
	}

	return nil
}

// GetCertificateFunc returns a function suitable for tls.Config.GetCertificate
// that reads the current certificate from the atomic value.
func (cm *CertManager) GetCertificateFunc() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, ok := cm.certValue.Load().(*tls.Certificate)
		if !ok || cert == nil {
			return nil, fmt.Errorf("no serving certificate available")
		}

		return cert, nil
	}
}

// CABundle returns the PEM-encoded CA certificate. This is used to inject
// caBundle into webhook and APIService configurations so the API server
// can verify the controller's self-signed serving certificate.
func (cm *CertManager) CABundle() []byte {
	if b, ok := cm.caBundle.Load().([]byte); ok {
		return b
	}

	return nil
}

// HMACKey returns the HMAC signing key stored in the serving Secret. The
// key is generated once during the first certificate rotation and preserved
// across subsequent rotations so that outstanding tokens remain valid.
func (cm *CertManager) HMACKey() []byte {
	if b, ok := cm.hmacKey.Load().([]byte); ok {
		return b
	}

	return nil
}

// RunRotationMonitor runs a loop that checks the current certificate every 24
// hours and rotates it if it is within 30 days of expiry. It blocks until the
// context is cancelled.
func (cm *CertManager) RunRotationMonitor(ctx context.Context) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Certificate rotation monitor stopped")
			return
		case <-ticker.C:
			cert, ok := cm.certValue.Load().(*tls.Certificate)
			if !ok || cert == nil {
				klog.Warningf("No certificate loaded, attempting rotation")

				if err := cm.rotateCertificate(ctx); err != nil {
					klog.Errorf("Certificate rotation failed: %v", err)
				}

				continue
			}

			leaf, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				klog.Errorf("Failed to parse current certificate: %v", err)
				continue
			}

			if !cm.validateCertificate(leaf) {
				klog.Infof("Certificate needs rotation, initiating")

				if err := cm.rotateCertificate(ctx); err != nil {
					klog.Errorf("Certificate rotation failed: %v", err)
				}
			}
		}
	}
}

// rotateCertificate loads or generates a CA, issues a new server certificate,
// stores everything in the Secret, publishes the CA to the ConfigMap, and
// updates the in-memory atomic value.
func (cm *CertManager) rotateCertificate(ctx context.Context) error {
	caKey, caCert, caCertPEM, caKeyPEM, err := cm.loadOrCreateCA(ctx)
	if err != nil {
		return fmt.Errorf("failed to load or create CA: %w", err)
	}

	// Generate a new server key.
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate server RSA key: %w", err)
	}

	serialNumber, err := randomSerialNumber()
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	now := time.Now()
	serverTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("%s.%s.svc", cm.serviceName, cm.namespace),
		},
		DNSNames:              cm.dnsNames(),
		IPAddresses:           cm.serviceIPs(ctx),
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(certValidityDuration),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("failed to sign server certificate: %w", err)
	}

	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(serverKey),
	})

	tlsCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return fmt.Errorf("failed to build TLS certificate: %w", err)
	}

	// Store CA + server cert and keys in Secret.
	// Preserve or generate the HMAC signing key. The key is created once
	// and reused across certificate rotations so outstanding tokens stay valid.
	var hmacKeyData []byte

	existing, err := cm.clientset.CoreV1().Secrets(cm.namespace).Get(ctx, cm.secretName, metav1.GetOptions{})
	if err == nil && len(existing.Data["hmac.key"]) >= authn.HMACKeySize {
		hmacKeyData = existing.Data["hmac.key"]

		klog.Infof("Preserving existing HMAC signing key from secret %s/%s", cm.namespace, cm.secretName)
	} else {
		hmacKeyData, err = authn.GenerateHMACKey()
		if err != nil {
			return fmt.Errorf("failed to generate HMAC key: %w", err)
		}

		klog.Infof("Generated new HMAC signing key for secret %s/%s", cm.namespace, cm.secretName)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cm.secretName,
			Namespace: cm.namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt":  serverCertPEM,
			"tls.key":  serverKeyPEM,
			"ca.crt":   caCertPEM,
			"ca.key":   caKeyPEM,
			"hmac.key": hmacKeyData,
		},
	}

	// Re-fetch existing secret for update (the earlier Get may have
	// been a not-found or the data reference could be stale after building
	// the new secret struct).
	existing, err = cm.clientset.CoreV1().Secrets(cm.namespace).Get(ctx, cm.secretName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing secret: %w", err)
		}

		_, err = cm.clientset.CoreV1().Secrets(cm.namespace).Create(ctx, secret, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create secret %s/%s: %w", cm.namespace, cm.secretName, err)
		}

		klog.Infof("Created secret %s/%s with new serving certificate", cm.namespace, cm.secretName)
	} else {
		existing.Data = secret.Data

		_, err = cm.clientset.CoreV1().Secrets(cm.namespace).Update(ctx, existing, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update secret %s/%s: %w", cm.namespace, cm.secretName, err)
		}

		klog.Infof("Updated secret %s/%s with new serving certificate", cm.namespace, cm.secretName)
	}

	// Publish CA cert (public part only) to ConfigMap.
	if err := cm.publishCA(ctx, caCertPEM); err != nil {
		return fmt.Errorf("failed to publish CA ConfigMap: %w", err)
	}

	cm.certValue.Store(&tlsCert)
	cm.caBundle.Store(append([]byte(nil), caCertPEM...))
	cm.hmacKey.Store(append([]byte(nil), hmacKeyData...))

	return nil
}

// loadOrCreateCA attempts to load an existing CA from the Secret. If the CA is
// missing or expired, a new self-signed CA is generated. It returns the CA
// private key, parsed certificate, and PEM-encoded cert and key.
func (cm *CertManager) loadOrCreateCA(ctx context.Context) (*rsa.PrivateKey, *x509.Certificate, []byte, []byte, error) {
	secret, err := cm.clientset.CoreV1().Secrets(cm.namespace).Get(ctx, cm.secretName, metav1.GetOptions{})
	if err == nil {
		caCertPEM := secret.Data["ca.crt"]

		caKeyPEM := secret.Data["ca.key"]
		if len(caCertPEM) > 0 && len(caKeyPEM) > 0 {
			caCert, certErr := parseCertPEM(caCertPEM)

			caKey, keyErr := parseRSAKeyPEM(caKeyPEM)
			if certErr == nil && keyErr == nil && time.Until(caCert.NotAfter) > caRotationThreshold {
				klog.Infof("Loaded existing CA (expires %s)", caCert.NotAfter.Format(time.RFC3339))
				return caKey, caCert, caCertPEM, caKeyPEM, nil
			}

			if certErr != nil {
				klog.Warningf("Failed to parse existing CA cert: %v", certErr)
			}

			if keyErr != nil {
				klog.Warningf("Failed to parse existing CA key: %v", keyErr)
			}

			if certErr == nil && time.Until(caCert.NotAfter) <= caRotationThreshold {
				klog.Infof("Existing CA expires within rotation threshold, generating new CA")
			}
		}
	}

	klog.Infof("Generating new self-signed CA")

	return cm.generateCA()
}

// generateCA creates a new self-signed CA with a 10-year validity period.
func (cm *CertManager) generateCA() (*rsa.PrivateKey, *x509.Certificate, []byte, []byte, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to generate CA key: %w", err)
	}

	serialNumber, err := randomSerialNumber()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "unbounded-net-ca",
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(caValidityDuration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	caKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caKey),
	})

	klog.Infof("Generated new CA (expires %s)", caCert.NotAfter.Format(time.RFC3339))

	return caKey, caCert, caCertPEM, caKeyPEM, nil
}

// publishCA creates or updates the CA ConfigMap with the CA certificate PEM.
// Only the public certificate is published -- the private key is never stored
// in the ConfigMap.
func (cm *CertManager) publishCA(ctx context.Context, caCertPEM []byte) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cm.caConfigMap,
			Namespace: cm.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "unbounded-net-controller",
				"app.kubernetes.io/component": "controller",
			},
		},
		Data: map[string]string{
			"ca.crt": string(caCertPEM),
		},
	}

	existing, err := cm.clientset.CoreV1().ConfigMaps(cm.namespace).Get(ctx, cm.caConfigMap, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing ConfigMap: %w", err)
		}

		_, err = cm.clientset.CoreV1().ConfigMaps(cm.namespace).Create(ctx, configMap, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create ConfigMap %s/%s: %w", cm.namespace, cm.caConfigMap, err)
		}

		klog.Infof("Created ConfigMap %s/%s with CA certificate", cm.namespace, cm.caConfigMap)

		return nil
	}

	existing.Data = configMap.Data
	existing.Labels = configMap.Labels

	_, err = cm.clientset.CoreV1().ConfigMaps(cm.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ConfigMap %s/%s: %w", cm.namespace, cm.caConfigMap, err)
	}

	klog.Infof("Updated ConfigMap %s/%s with CA certificate", cm.namespace, cm.caConfigMap)

	return nil
}

// dnsNames returns the DNS SAN entries for the serving certificate.
func (cm *CertManager) dnsNames() []string {
	return []string{
		fmt.Sprintf("%s.%s.svc", cm.serviceName, cm.namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", cm.serviceName, cm.namespace),
		fmt.Sprintf("%s.%s", cm.serviceName, cm.namespace),
		cm.serviceName,
	}
}

// serviceIPs looks up the ClusterIP(s) of the controller service and returns
// them as net.IP values for use as IP SANs in the serving certificate.
func (cm *CertManager) serviceIPs(ctx context.Context) []net.IP {
	svc, err := cm.clientset.CoreV1().Services(cm.namespace).Get(ctx, cm.serviceName, metav1.GetOptions{})
	if err != nil {
		klog.V(2).Infof("Could not look up service %s/%s for IP SANs: %v", cm.namespace, cm.serviceName, err)
		return nil
	}

	var ips []net.IP
	if ip := net.ParseIP(svc.Spec.ClusterIP); ip != nil {
		ips = append(ips, ip)
	}

	for _, cipStr := range svc.Spec.ClusterIPs {
		if ip := net.ParseIP(cipStr); ip != nil {
			// Deduplicate against the primary ClusterIP.
			dup := false

			for _, existing := range ips {
				if existing.Equal(ip) {
					dup = true
					break
				}
			}

			if !dup {
				ips = append(ips, ip)
			}
		}
	}

	if len(ips) > 0 {
		names := make([]string, len(ips))
		for i, ip := range ips {
			names[i] = ip.String()
		}

		klog.Infof("Including service ClusterIP(s) as IP SANs: %v", names)
	}

	return ips
}

// validateCertificate checks that the certificate is not expired, not expiring
// within the rotation threshold, and contains the expected DNS SANs.
func (cm *CertManager) validateCertificate(cert *x509.Certificate) bool {
	now := time.Now()

	if now.After(cert.NotAfter) {
		klog.Infof("Certificate has expired (NotAfter: %s)", cert.NotAfter.Format(time.RFC3339))
		return false
	}

	if cert.NotAfter.Sub(now) < rotationThreshold {
		klog.Infof("Certificate expires within rotation threshold (NotAfter: %s, threshold: %s)",
			cert.NotAfter.Format(time.RFC3339), rotationThreshold)

		return false
	}

	expectedDNS := cm.dnsNames()

	certDNS := make(map[string]bool)
	for _, name := range cert.DNSNames {
		certDNS[name] = true
	}

	for _, expected := range expectedDNS {
		if !certDNS[expected] {
			klog.Infof("Certificate missing expected DNS SAN: %s", expected)
			return false
		}
	}

	return true
}

// parseCertPEM parses a PEM-encoded certificate.
func parseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	return x509.ParseCertificate(block.Bytes)
}

// parseRSAKeyPEM parses a PEM-encoded RSA private key.
func parseRSAKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// randomSerialNumber generates a random serial number for x509 certificates.
func randomSerialNumber() (*big.Int, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, serialNumberLimit)
}
