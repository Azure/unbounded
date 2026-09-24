// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/e2e/racer/fixture"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

// The SDK talks ordinary volume HTTP, but a production daemon must enroll its
// locally generated key when subscribing to HTTPS control.
func dataplaneEnrollment(t *testing.T, dir string) []string {
	t.Helper()

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SDK fixture root"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}

	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}

	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}

	rootDigest := sha256.Sum256(rootDER)
	bundle := racermeta.TrustBundle{Version: 1, Generation: 1, Active: hex.EncodeToString(rootDigest[:]), Certificates: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))}
	// This test-only issuer signs the daemon's own CSR, without a Go CA manager.
	issue := func(public any, uri string, client bool) ([]byte, error) {
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
		if err != nil {
			return nil, err
		}

		identity, err := url.Parse(uri)
		if err != nil {
			return nil, err
		}

		leaf := &x509.Certificate{SerialNumber: serial, NotBefore: root.NotBefore, NotAfter: root.NotAfter, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{identity}}
		if client {
			leaf.ExtKeyUsage = append(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		} else {
			leaf.DNSNames = []string{"racer-controlplane.sdk.svc"}
		}

		return x509.CreateCertificate(rand.Reader, leaf, root, public, rootKey)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	serverClaims := fixture.Claims{Version: 1, Namespace: "sdk", Identity: fixture.CertificateIdentity{Kind: "controlplane", PodUID: "controller", BootID: strings.Repeat("04", 32), PodName: "controller"}}

	serverDER, err := issue(key.Public(), serverClaims.URI(), false)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(root)

	nodeClaims := func(boot string) fixture.Claims {
		return fixture.Claims{Version: 1, Namespace: "sdk", Identity: fixture.CertificateIdentity{Kind: "node", Universe: strings.Repeat("01", 32), Node: strings.Repeat("02", 32), PodUID: "sdk-pod", BootID: boot, PodName: "dataplane"}}
	}
	authenticated := func(r *http.Request) bool {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.PeerCertificates[0].URIs) != 1 {
			return false
		}

		claims, err := fixture.ParseClaims(r.TLS.PeerCertificates[0].URIs[0].String())

		return err == nil && claims == nodeClaims(r.Header.Get("X-Racer-Boot"))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(r) {
			http.Error(w, "client certificate required", http.StatusForbidden)
			return
		}

		boot, err := hex.DecodeString(r.Header.Get("X-Racer-Boot"))
		if err != nil || len(boot) != 32 {
			http.Error(w, "invalid boot", http.StatusBadRequest)
			return
		}

		deadline := time.NewTimer(28 * time.Second)
		defer deadline.Stop()

		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()

		for {
			wire, err := os.ReadFile(filepath.Join(dir, "config.json"))

			var config pb.Configuration
			if err != nil || protojson.Unmarshal(wire, &config) != nil || config.GetSnapshot() == nil {
				http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
				return
			}

			snapshot := config.GetSnapshot()
			raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(snapshot)
			digest := sha256.Sum256(raw)

			cursor := hex.EncodeToString(digest[:])
			if r.Header.Get("X-Racer-Cursor") == cursor {
				select {
				case <-r.Context().Done():
					return
				case <-deadline.C:
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(http.StatusNoContent)

					return
				case <-tick.C:
					continue
				}
			}

			body, _ := proto.Marshal(&pb.DesiredState{Universe: snapshot.Universe, Node: snapshot.Node, Incarnation: boot, SnapshotDigest: digest[:], Revision: snapshot.Revision, Configuration: &config, Profile: 1, PodUid: "sdk-pod", Cursor: cursor})

			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)

			return
		}
	})
	mux.HandleFunc("POST /v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			CSR       string `json:"csr"`
			Namespace string `json:"pod_namespace"`
			Pod       string `json:"pod_name"`
			Universe  string `json:"expected_universe"`
			Node      string `json:"expected_node"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Namespace != "sdk" || request.Pod != "dataplane" || request.Universe != strings.Repeat("01", 32) || request.Node != strings.Repeat("02", 32) || r.Header.Get("Authorization") != "Bearer sdk-test-token" {
			t.Logf("enrollment identity: namespace=%q pod=%q universe=%q node=%q error=%v", request.Namespace, request.Pod, request.Universe, request.Node, err)
			http.Error(w, "invalid enrollment", http.StatusForbidden)

			return
		}

		block, rest := pem.Decode([]byte(request.CSR))
		if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
			http.Error(w, "invalid CSR", http.StatusBadRequest)
			return
		}

		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			http.Error(w, "invalid CSR signature", http.StatusBadRequest)
			return
		}

		boot := r.Header.Get("X-Racer-Boot")

		decodedBoot, err := hex.DecodeString(boot)
		if err != nil || len(decodedBoot) != 32 || hex.EncodeToString(decodedBoot) != boot {
			http.Error(w, "invalid boot", http.StatusBadRequest)
			return
		}

		leaf, err := issue(csr.PublicKey, nodeClaims(boot).URI(), true)
		if err != nil {
			t.Error(err)
			http.Error(w, "issuance failed", http.StatusInternalServerError)

			return
		}

		body, _ := json.Marshal(map[string]any{"certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf})), "generation": bundle.Generation, "issuer": bundle.Active})

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("POST /v1/proof", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(r) {
			http.Error(w, "client certificate required", http.StatusForbidden)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: key}}, ClientCAs: roots, ClientAuth: tls.VerifyClientCertIfGiven}
	server.StartTLS()
	t.Cleanup(server.Close)

	for name, data := range map[string][]byte{"bundle.json": bundle.JSON(), "token": []byte("sdk-test-token")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return []string{
		"RACER_CONTROL_PLANE_URL=" + server.URL + "/v1/config",
		"RACER_TLS_TRUST_DIR=" + dir,
		"RACER_ENROLL_URL=" + server.URL + "/v1/enroll",
		"RACER_CONTROL_SERVER_NAME=racer-controlplane.sdk.svc",
		"RACER_CONTROL_TOKEN_FILE=" + filepath.Join(dir, "token"),
		"RACER_POD_NAMESPACE=sdk", "RACER_POD_NAME=dataplane", "RACER_POD_UID=sdk-pod",
	}
}
