// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/e2e/racer/fixture"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func TestDataplaneEnrollment(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{}

	for _, entry := range dataplaneEnrollment(t, dir) {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}

	wire, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}

	var bundle racermeta.TrustBundle
	if err := json.Unmarshal(wire, &bundle); err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(bundle.Certificates)) {
		t.Fatal("invalid fixture trust bundle")
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: env["RACER_CONTROL_SERVER_NAME"]}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// The issuer must derive claims from the authorized request, not CSR SANs.
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{{Scheme: "spiffe", Host: "racer", Path: "/controlplane"}}}, key)
	if err != nil {
		t.Fatal(err)
	}

	boot := strings.Repeat("ab", 32)
	request := map[string]string{
		"csr":           string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		"pod_namespace": "sdk", "pod_name": "dataplane",
		"expected_universe": strings.Repeat("01", 32), "expected_node": strings.Repeat("02", 32),
	}
	enroll := func(t *testing.T, body map[string]string, token, boot string, want int) []byte {
		t.Helper()

		wire, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}

		r, err := http.NewRequest(http.MethodPost, env["RACER_ENROLL_URL"], bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Racer-Boot", boot)

		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != want {
			t.Fatalf("enrollment status: got %d want %d", resp.StatusCode, want)
		}

		if want != http.StatusOK {
			return nil
		}

		var reply struct {
			Certificate string `json:"certificate"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}

		block, _ := pem.Decode([]byte(reply.Certificate))
		if block == nil {
			t.Fatal("missing enrolled certificate")
		}

		return block.Bytes
	}

	for _, field := range []string{"pod_namespace", "pod_name", "expected_universe", "expected_node", "csr"} {
		t.Run(field, func(t *testing.T) {
			body := make(map[string]string, len(request))
			for key, value := range request {
				body[key] = value
			}

			body[field] = "wrong"

			want := http.StatusForbidden
			if field == "csr" {
				want = http.StatusBadRequest
			}

			enroll(t, body, "sdk-test-token", boot, want)
		})
	}

	t.Run("wrong-token", func(t *testing.T) {
		enroll(t, request, "wrong", boot, http.StatusForbidden)
	})

	for _, badBoot := range []string{"", "zz", "ab", strings.ToUpper(boot)} {
		t.Run("invalid-boot-"+badBoot, func(t *testing.T) {
			enroll(t, request, "sdk-test-token", badBoot, http.StatusBadRequest)
		})
	}

	der := enroll(t, request, "sdk-test-token", boot, http.StatusOK)

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	wantClaims := fixture.Claims{Version: 1, Namespace: "sdk", Identity: fixture.CertificateIdentity{Kind: "node", Universe: request["expected_universe"], Node: request["expected_node"], PodUID: "sdk-pod", BootID: boot, PodName: "dataplane"}}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != wantClaims.URI() {
		t.Fatalf("unexpected enrolled claims: %v", leaf.URIs)
	}

	mtlsConfig := tlsConfig.Clone()
	mtlsConfig.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}
	mtlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: mtlsConfig}, Timeout: 5 * time.Second}
	t.Cleanup(mtlsClient.CloseIdleConnections)

	for _, endpoint := range []struct{ method, path string }{{http.MethodGet, "/v4/config"}, {http.MethodPost, "/v3/proof"}} {
		for _, tc := range []struct {
			name   string
			client *http.Client
			boot   string
		}{
			{"no-certificate", client, boot},
			{"wrong-boot", mtlsClient, strings.Repeat("cd", 32)},
			{"missing-boot", mtlsClient, ""},
			{"authorized", mtlsClient, boot},
		} {
			t.Run(endpoint.path+"/"+tc.name, func(t *testing.T) {
				r, err := http.NewRequest(endpoint.method, strings.TrimSuffix(env["RACER_ENROLL_URL"], "/v3/enroll")+endpoint.path, nil)
				if err != nil {
					t.Fatal(err)
				}

				r.Header.Set("X-Racer-Boot", tc.boot)

				resp, err := tc.client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()

				want := http.StatusForbidden
				if tc.name == "authorized" {
					want = http.StatusNoContent
					if endpoint.path == "/v4/config" {
						// Authentication succeeds, but this test has no configuration.
						want = http.StatusServiceUnavailable
					}
				}

				if resp.StatusCode != want {
					t.Fatalf("status: got %d want %d", resp.StatusCode, want)
				}
			})
		}
	}
}
