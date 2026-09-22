// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer/pki"
)

// The SDK talks ordinary volume HTTP, but a production daemon must enroll its
// locally generated key when subscribing to HTTPS control.
func dataplaneEnrollment(t *testing.T, dir string) []string {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	manager, err := pki.New(fake.NewClientBuilder().WithScheme(scheme).Build(), "sdk", pki.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.AcquireLeadership(t.Context(), "sdk-test"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}

	issued, err := manager.Issue(t.Context(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), pki.Identity{Kind: pki.ControlPlane, PodUID: "control", BootID: "sdk-test"})
	if err != nil {
		t.Fatal(err)
	}

	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	hot := pki.NewHotTLS()
	if err := hot.Update(issued.Bundle.JSON(), issued.CertificatePEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "client certificate required", http.StatusForbidden)
			return
		}

		wire, err := os.ReadFile(filepath.Join(dir, "config.json"))

		var config pb.Configuration
		if err != nil || protojson.Unmarshal(wire, &config) != nil {
			http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
			return
		}

		snapshot := config.GetSnapshot()
		raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(snapshot)
		digest := sha256.Sum256(raw)
		boot, _ := hex.DecodeString(r.Header.Get("X-Racer-Boot"))
		phase, _ := strconv.Atoi(r.Header.Get("X-Racer-Phase"))
		phase = min(phase+1, 4)
		body, _ := proto.Marshal(&pb.ControlCommand{Universe: snapshot.Universe, Node: snapshot.Node, Incarnation: boot, SnapshotDigest: digest[:], Revision: snapshot.Revision, Phase: uint32(phase), Configuration: &config, Profile: 1, PodUid: "sdk-pod"})

		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("POST /v3/enroll", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			CSR       string `json:"csr"`
			Namespace string `json:"pod_namespace"`
			Pod       string `json:"pod_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Namespace != "sdk" || request.Pod != "dataplane" || r.Header.Get("Authorization") != "Bearer sdk-test-token" {
			http.Error(w, "invalid enrollment", http.StatusForbidden)
			return
		}

		leaf, err := manager.Issue(r.Context(), []byte(request.CSR), pki.Identity{Kind: pki.Node, Universe: strings.Repeat("01", 32), Node: strings.Repeat("02", 32), PodUID: "sdk-pod", BootID: r.Header.Get("X-Racer-Boot")})
		if err != nil {
			t.Error(err)
			http.Error(w, "issuance failed", http.StatusInternalServerError)

			return
		}

		body, _ := json.Marshal(map[string]any{"certificate": string(leaf.CertificatePEM), "generation": leaf.Bundle.Generation, "issuer": leaf.RootDigest})

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	server := httptest.NewUnstartedServer(mux)
	server.TLS = hot.ServerConfig(tls.VerifyClientCertIfGiven)
	server.StartTLS()
	t.Cleanup(server.Close)

	for name, data := range map[string][]byte{pki.BundleKey: issued.Bundle.JSON(), "token": []byte("sdk-test-token")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return []string{
		"RACER_CONTROL_PLANE_URL=" + server.URL + "/v3/" + strings.Repeat("01", 32) + "/" + strings.Repeat("02", 32),
		"RACER_TLS_TRUST_DIR=" + dir,
		"RACER_ENROLL_URL=" + server.URL + "/v3/enroll",
		"RACER_CONTROL_SERVER_NAME=racer-controlplane.sdk.svc",
		"RACER_CONTROL_TOKEN_FILE=" + filepath.Join(dir, "token"),
		"RACER_POD_NAMESPACE=sdk", "RACER_POD_NAME=dataplane", "RACER_POD_UID=sdk-pod",
	}
}
