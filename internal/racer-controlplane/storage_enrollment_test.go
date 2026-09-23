// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/racer/pki"
)

// The production daemon creates its own key on every boot. Fixture enrollment
// remains available while the storage test suspends topology/policy delivery.
func storageEnrollment(t *testing.T, ca *coordinationPKI, pod *corev1.Pod, node string) []string {
	t.Helper()
	dir := t.TempDir()
	digest := sha256.Sum256(ca.root.Raw)

	bundle := pki.TrustBundle{Version: 1, Generation: 1, Active: hex.EncodeToString(digest[:]), Certificates: string(ca.pem)}
	for name, data := range map[string][]byte{pki.BundleKey: bundle.JSON(), "token": []byte("storage-test-token")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cert, key := ca.leaf(t, "spiffe://racer/controlplane", "localhost")

	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}

	uri, err := url.Parse("spiffe://racer/universe/" + identity("universe", "default") + "/node/" + node + "/pod/" + string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/enroll", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			CSR       string `json:"csr"`
			Namespace string `json:"pod_namespace"`
			Name      string `json:"pod_name"`
		}

		boot, err := hex.DecodeString(req.Header.Get("X-Racer-Boot"))
		if err != nil || len(boot) != 32 || req.Header.Get("Authorization") != "Bearer storage-test-token" || json.NewDecoder(http.MaxBytesReader(w, req.Body, 32768)).Decode(&body) != nil || body.Namespace != pod.Namespace || body.Name != pod.Name {
			http.Error(w, "invalid enrollment identity", http.StatusForbidden)
			return
		}

		block, _ := pem.Decode([]byte(body.CSR))
		if block == nil || block.Type != "CERTIFICATE REQUEST" {
			http.Error(w, "invalid CSR PEM", http.StatusBadRequest)
			return
		}

		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			http.Error(w, "invalid CSR", http.StatusBadRequest)
			return
		}

		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			t.Error(err)
			http.Error(w, "serial unavailable", http.StatusInternalServerError)

			return
		}

		leaf := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{uri}}

		der, err := x509.CreateCertificate(rand.Reader, leaf, ca.root, csr.PublicKey, ca.key)
		if err != nil {
			t.Error(err)
			http.Error(w, "issuance failed", http.StatusInternalServerError)

			return
		}

		response, _ := json.Marshal(enrollmentResponse{Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Generation: bundle.Generation, Issuer: bundle.Active})

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	})
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	t.Cleanup(server.Close)

	return []string{
		"RACER_TLS_TRUST_DIR=" + dir,
		"RACER_ENROLL_URL=" + server.URL + "/v3/enroll",
		"RACER_CONTROL_TOKEN_FILE=" + filepath.Join(dir, "token"),
		"RACER_POD_NAMESPACE=" + pod.Namespace, "RACER_POD_NAME=" + pod.Name,
	}
}
