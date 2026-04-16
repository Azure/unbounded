// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package certmanager

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// generateTestCA creates a self-signed CA certificate and key for testing,
// returning PEM-encoded cert and key bytes along with the parsed objects.
func generateTestCA(t *testing.T, notBefore, notAfter time.Time) (caCertPEM, caKeyPEM []byte, caCert *x509.Certificate, caKey *rsa.PrivateKey) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "unbounded-net-ca",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("failed to create CA certificate: %v", err)
	}

	caCert, err = x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("failed to parse CA certificate: %v", err)
	}

	caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	caKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})

	return caCertPEM, caKeyPEM, caCert, caKey
}

// generateTestCert creates a server certificate signed by the given CA,
// returning PEM-encoded cert and key bytes.
func generateTestCert(t *testing.T, caCert *x509.Certificate, caKey *rsa.PrivateKey, dnsNames []string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			CommonName: "test",
		},
		DNSNames:    dnsNames,
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return certPEM, keyPEM
}

func TestNewCertManagerDefaults(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "kube-system",
		ServiceName: "test-service",
	})

	if cm.secretName != defaultSecretName {
		t.Errorf("expected default secret name %q, got %q", defaultSecretName, cm.secretName)
	}

	if cm.caConfigMap != defaultCAConfigMap {
		t.Errorf("expected default CA ConfigMap name %q, got %q", defaultCAConfigMap, cm.caConfigMap)
	}

	if cm.namespace != "kube-system" {
		t.Errorf("expected namespace %q, got %q", "kube-system", cm.namespace)
	}

	if cm.serviceName != "test-service" {
		t.Errorf("expected service name %q, got %q", "test-service", cm.serviceName)
	}
}

func TestNewCertManagerCustomNames(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "default",
		ServiceName: "my-svc",
		SecretName:  "custom-secret",
		CAConfigMap: "custom-ca-cm",
	})

	if cm.secretName != "custom-secret" {
		t.Errorf("expected secret name %q, got %q", "custom-secret", cm.secretName)
	}

	if cm.caConfigMap != "custom-ca-cm" {
		t.Errorf("expected CA ConfigMap name %q, got %q", "custom-ca-cm", cm.caConfigMap)
	}
}

func TestDNSNames(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "kube-system",
		ServiceName: "unbounded-net-controller",
	})

	names := cm.dnsNames()
	expected := []string{
		"unbounded-net-controller.kube-system.svc",
		"unbounded-net-controller.kube-system.svc.cluster.local",
		"unbounded-net-controller.kube-system",
		"unbounded-net-controller",
	}

	if len(names) != len(expected) {
		t.Fatalf("expected %d DNS names, got %d: %v", len(expected), len(names), names)
	}

	for i, name := range names {
		if name != expected[i] {
			t.Errorf("DNS name[%d]: expected %q, got %q", i, expected[i], name)
		}
	}
}

func TestValidateCertificate(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	expectedDNS := cm.dnsNames()

	tests := []struct {
		name      string
		dns       []string
		notBefore time.Time
		notAfter  time.Time
		valid     bool
	}{
		{
			name:      "valid certificate",
			dns:       expectedDNS,
			notBefore: time.Now().Add(-1 * time.Hour),
			notAfter:  time.Now().Add(90 * 24 * time.Hour),
			valid:     true,
		},
		{
			name:      "expired certificate",
			dns:       expectedDNS,
			notBefore: time.Now().Add(-48 * time.Hour),
			notAfter:  time.Now().Add(-24 * time.Hour),
			valid:     false,
		},
		{
			name:      "expiring within threshold",
			dns:       expectedDNS,
			notBefore: time.Now().Add(-1 * time.Hour),
			notAfter:  time.Now().Add(15 * 24 * time.Hour), // 15 days < 30 day threshold
			valid:     false,
		},
		{
			name:      "missing SANs",
			dns:       []string{"wrong.example.com"},
			notBefore: time.Now().Add(-1 * time.Hour),
			notAfter:  time.Now().Add(90 * 24 * time.Hour),
			valid:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("failed to generate key: %v", err)
			}

			template := &x509.Certificate{
				SerialNumber: big.NewInt(1),
				Subject:      pkix.Name{CommonName: "test"},
				DNSNames:     tc.dns,
				NotBefore:    tc.notBefore,
				NotAfter:     tc.notAfter,
			}

			certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatalf("failed to create certificate: %v", err)
			}

			cert, err := x509.ParseCertificate(certDER)
			if err != nil {
				t.Fatalf("failed to parse certificate: %v", err)
			}

			result := cm.validateCertificate(cert)
			if result != tc.valid {
				t.Errorf("expected validateCertificate to return %v, got %v", tc.valid, result)
			}
		})
	}
}

func TestRotateCertificateCreatesCAAndServerCert(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	ctx := context.Background()
	if err := cm.rotateCertificate(ctx); err != nil {
		t.Fatalf("rotateCertificate: %v", err)
	}

	// Verify the certificate was loaded into the atomic value.
	loaded, ok := cm.certValue.Load().(*tls.Certificate)
	if !ok || loaded == nil {
		t.Fatal("expected certificate to be loaded into certValue")
	}

	// Verify the Secret was created with all expected fields.
	secret, err := client.CoreV1().Secrets("kube-system").Get(ctx, defaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get secret: %v", err)
	}

	for _, field := range []string{"tls.crt", "tls.key", "ca.crt", "ca.key"} {
		if len(secret.Data[field]) == 0 {
			t.Errorf("secret missing field %q", field)
		}
	}

	// Parse the CA cert and verify it is a CA.
	caCert, err := parseCertPEM(secret.Data["ca.crt"])
	if err != nil {
		t.Fatalf("failed to parse CA cert: %v", err)
	}

	if !caCert.IsCA {
		t.Error("CA certificate is not marked as CA")
	}

	if caCert.Subject.CommonName != "unbounded-net-ca" {
		t.Errorf("expected CA CN %q, got %q", "unbounded-net-ca", caCert.Subject.CommonName)
	}

	// Parse the server cert and verify SANs.
	serverCert, err := parseCertPEM(secret.Data["tls.crt"])
	if err != nil {
		t.Fatalf("failed to parse server cert: %v", err)
	}

	expectedDNS := cm.dnsNames()
	if len(serverCert.DNSNames) != len(expectedDNS) {
		t.Fatalf("expected %d DNS SANs, got %d", len(expectedDNS), len(serverCert.DNSNames))
	}

	// Verify the server cert is signed by the CA.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	if _, err := serverCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   expectedDNS[0],
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("server certificate verification failed: %v", err)
	}
}

func TestRotateCertificatePublishesCAConfigMap(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	ctx := context.Background()
	if err := cm.rotateCertificate(ctx); err != nil {
		t.Fatalf("rotateCertificate: %v", err)
	}

	// Verify the ConfigMap was created.
	configMap, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, defaultCAConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get CA ConfigMap: %v", err)
	}

	caPEM := configMap.Data["ca.crt"]
	if caPEM == "" {
		t.Fatal("CA ConfigMap missing ca.crt")
	}

	// Verify the CA cert in ConfigMap matches the one in the Secret.
	secret, err := client.CoreV1().Secrets("kube-system").Get(ctx, defaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get secret: %v", err)
	}

	if caPEM != string(secret.Data["ca.crt"]) {
		t.Error("CA cert in ConfigMap does not match CA cert in Secret")
	}

	// Verify the CA private key is NOT in the ConfigMap.
	if _, hasKey := configMap.Data["ca.key"]; hasKey {
		t.Error("CA ConfigMap should not contain ca.key")
	}

	// Verify labels.
	if configMap.Labels["app.kubernetes.io/name"] != "unbounded-net-controller" {
		t.Errorf("expected label app.kubernetes.io/name=unbounded-net-controller, got %q",
			configMap.Labels["app.kubernetes.io/name"])
	}
}

func TestEnsureCertificateLoadsFromSecret(t *testing.T) {
	// Pre-generate a CA and server cert, store them in a Secret, and verify
	// EnsureCertificate loads without regenerating.
	cm := NewCertManager(Options{
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	dns := cm.dnsNames()
	caCertPEM, caKeyPEM, caCert, caKey := generateTestCA(t,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(10*365*24*time.Hour))

	certPEM, keyPEM := generateTestCert(t, caCert, caKey, dns,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(90*24*time.Hour))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultSecretName,
			Namespace: "kube-system",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caCertPEM,
			"ca.key":  caKeyPEM,
		},
	}

	client := fake.NewClientset(secret)
	cm.clientset = client

	ctx := context.Background()
	if err := cm.EnsureCertificate(ctx); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}

	// Verify the certificate was loaded into the atomic value.
	loaded, ok := cm.certValue.Load().(*tls.Certificate)
	if !ok || loaded == nil {
		t.Fatal("expected certificate to be loaded into certValue")
	}

	// Verify the CA ConfigMap was published.
	configMap, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, defaultCAConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get CA ConfigMap: %v", err)
	}

	if configMap.Data["ca.crt"] != string(caCertPEM) {
		t.Error("CA ConfigMap ca.crt does not match expected CA")
	}
}

func TestEnsureCertificateRotatesExpiredServerCertReusesCA(t *testing.T) {
	cm := NewCertManager(Options{
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	dns := cm.dnsNames()
	caCertPEM, caKeyPEM, caCert, caKey := generateTestCA(t,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(10*365*24*time.Hour))

	// Create an expired server certificate.
	certPEM, keyPEM := generateTestCert(t, caCert, caKey, dns,
		time.Now().Add(-48*time.Hour),
		time.Now().Add(-1*time.Hour))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultSecretName,
			Namespace: "kube-system",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caCertPEM,
			"ca.key":  caKeyPEM,
		},
	}

	client := fake.NewClientset(secret)
	cm.clientset = client

	ctx := context.Background()
	if err := cm.EnsureCertificate(ctx); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}

	// Verify a new certificate was generated and loaded.
	loaded, ok := cm.certValue.Load().(*tls.Certificate)
	if !ok || loaded == nil {
		t.Fatal("expected certificate to be loaded into certValue")
	}

	// Verify the CA was reused (same CA cert in the Secret).
	updatedSecret, err := client.CoreV1().Secrets("kube-system").Get(ctx, defaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated secret: %v", err)
	}

	if string(updatedSecret.Data["ca.crt"]) != string(caCertPEM) {
		t.Error("expected CA cert to be reused, but it was regenerated")
	}

	// Verify the new server cert is valid and signed by the same CA.
	newServerCert, err := parseCertPEM(updatedSecret.Data["tls.crt"])
	if err != nil {
		t.Fatalf("failed to parse new server cert: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	if _, err := newServerCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   dns[0],
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("new server certificate verification failed: %v", err)
	}
}

func TestEnsureCertificateRegeneratesExpiredCA(t *testing.T) {
	cm := NewCertManager(Options{
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	dns := cm.dnsNames()
	// Create an expired CA.
	caCertPEM, caKeyPEM, caCert, caKey := generateTestCA(t,
		time.Now().Add(-48*time.Hour),
		time.Now().Add(-1*time.Hour))

	certPEM, keyPEM := generateTestCert(t, caCert, caKey, dns,
		time.Now().Add(-48*time.Hour),
		time.Now().Add(-1*time.Hour))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultSecretName,
			Namespace: "kube-system",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caCertPEM,
			"ca.key":  caKeyPEM,
		},
	}

	client := fake.NewClientset(secret)
	cm.clientset = client

	ctx := context.Background()
	if err := cm.EnsureCertificate(ctx); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}

	// Verify a new CA was generated (different from the original).
	updatedSecret, err := client.CoreV1().Secrets("kube-system").Get(ctx, defaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated secret: %v", err)
	}

	if string(updatedSecret.Data["ca.crt"]) == string(caCertPEM) {
		t.Error("expected CA cert to be regenerated, but it was the same")
	}

	// Verify the new CA is valid.
	newCACert, err := parseCertPEM(updatedSecret.Data["ca.crt"])
	if err != nil {
		t.Fatalf("failed to parse new CA cert: %v", err)
	}

	if !newCACert.IsCA {
		t.Error("new CA certificate is not marked as CA")
	}

	if time.Now().After(newCACert.NotAfter) {
		t.Error("new CA certificate is already expired")
	}
}

func TestGetCertificateFunc(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	getCert := cm.GetCertificateFunc()

	// No cert loaded yet -- should return error.
	_, err := getCert(nil)
	if err == nil {
		t.Fatal("expected error when no certificate is loaded")
	}

	// Generate a CA and server cert and store it.
	caCertPEM, _, caCert, caKey := generateTestCA(t,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(10*365*24*time.Hour))
	_ = caCertPEM

	dns := cm.dnsNames()
	certPEM, keyPEM := generateTestCert(t, caCert, caKey, dns,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(90*24*time.Hour))

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("failed to create TLS key pair: %v", err)
	}

	cm.certValue.Store(&tlsCert)

	// Now should return the cert.
	result, err := getCert(nil)
	if err != nil {
		t.Fatalf("GetCertificateFunc returned error: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil certificate")
	}

	if len(result.Certificate) == 0 {
		t.Fatal("expected certificate chain to be non-empty")
	}
}

func TestEnsureCertificateNoSecretTriggersFullGeneration(t *testing.T) {
	client := fake.NewClientset()
	cm := NewCertManager(Options{
		Clientset:   client,
		Namespace:   "kube-system",
		ServiceName: "test-svc",
	})

	ctx := context.Background()
	if err := cm.EnsureCertificate(ctx); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}

	// Verify the Secret was created.
	secret, err := client.CoreV1().Secrets("kube-system").Get(ctx, defaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get secret: %v", err)
	}

	for _, field := range []string{"tls.crt", "tls.key", "ca.crt", "ca.key"} {
		if len(secret.Data[field]) == 0 {
			t.Errorf("secret missing field %q", field)
		}
	}

	// Verify the ConfigMap was created.
	_, err = client.CoreV1().ConfigMaps("kube-system").Get(ctx, defaultCAConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get CA ConfigMap: %v", err)
	}

	// Verify the certificate was loaded.
	loaded, ok := cm.certValue.Load().(*tls.Certificate)
	if !ok || loaded == nil {
		t.Fatal("expected certificate to be loaded into certValue")
	}
}
