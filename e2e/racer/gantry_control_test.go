//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func TestGantryControlFixture(t *testing.T) {
	f := &gantryFixture{t: t, dir: t.TempDir()}
	env := map[string]string{}

	for _, entry := range f.control(2)[0] {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}

	wire, err := os.ReadFile(filepath.Join(f.dir, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := racermeta.ParseTrustBundle(wire)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(bundle.Certificates)) {
		t.Fatal("missing fixture CA")
	}

	token, err := os.ReadFile(env["RACER_CONTROL_TOKEN_FILE"])
	if err != nil {
		t.Fatal(err)
	}

	newClient := func(config *tls.Config) *http.Client {
		transport := &http.Transport{TLSClientConfig: config}
		t.Cleanup(transport.CloseIdleConnections)

		return &http.Client{Transport: transport, Timeout: 2 * time.Second}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: env["RACER_CONTROL_SERVER_NAME"]}
	client := newClient(tlsConfig)
	request := func(client *http.Client, method, endpoint, auth, boot string, body []byte, status int) []byte {
		t.Helper()

		r, err := http.NewRequestWithContext(t.Context(), method, endpoint, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Authorization", auth)
		r.Header.Set("X-Racer-Boot", boot)

		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("%s %s: status=%d body=%s err=%v", method, endpoint, resp.StatusCode, data, err)
		}

		if status == http.StatusOK && method == http.MethodGet && (resp.ContentLength != int64(len(data)) || len(resp.TransferEncoding) != 0) {
			t.Fatal("desired state must use explicit Content-Length")
		}

		return data
	}
	boot := strings.Repeat("03", 32)
	controlURL := env["RACER_CONTROL_PLANE_URL"]
	proofURL := strings.TrimSuffix(env["RACER_ENROLL_URL"], "enroll") + "proof"

	request(client, "GET", controlURL, "", boot, nil, http.StatusForbidden)
	request(client, "POST", proofURL, "", boot, nil, http.StatusForbidden)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{{Scheme: "spiffe", Host: "racer", Path: "/controlplane"}}}, key)
	if err != nil {
		t.Fatal(err)
	}

	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
	badSignature := bytes.Clone(csr)

	badSignature[len(badSignature)-1] ^= 1
	for _, tc := range []struct {
		name, namespace, pod, token, csr string
		status                           int
	}{
		{"token", "gantry-e2e", "node0", "wrong", csrPEM, 403},
		{"namespace", "wrong", "node0", string(token), csrPEM, 403},
		{"pod", "gantry-e2e", "node2", string(token), csrPEM, 403},
		{"pod-prefix", "gantry-e2e", "0", string(token), csrPEM, 403},
		{"csr-framing", "gantry-e2e", "node0", string(token), csrPEM + "junk", 400},
		{"csr-signature", "gantry-e2e", "node0", string(token), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: badSignature})), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"csr": tc.csr, "pod_namespace": tc.namespace, "pod_name": tc.pod})
			request(client, "POST", env["RACER_ENROLL_URL"], "Bearer "+tc.token, boot, body, tc.status)
		})
	}

	for i := range 2 {
		body, _ := json.Marshal(map[string]string{"csr": csrPEM, "pod_namespace": "gantry-e2e", "pod_name": fmt.Sprintf("node%d", i)})
		data := request(client, "POST", env["RACER_ENROLL_URL"], "Bearer "+string(token), boot, body, 200)

		var issued struct {
			Certificate string `json:"certificate"`
			Generation  uint64 `json:"generation"`
			Issuer      string `json:"issuer"`
		}
		if err := json.Unmarshal(data, &issued); err != nil || issued.Generation != bundle.Generation || issued.Issuer != bundle.Active {
			t.Fatalf("invalid enrollment metadata: %s err=%v", data, err)
		}

		block, _ := pem.Decode([]byte(issued.Certificate))
		if block == nil {
			t.Fatal("missing node certificate")
		}

		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}

		for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
				t.Fatal(err)
			}
		}

		uri := "spiffe://racer/universe/" + strings.Repeat("01", 32) + "/node/" + gantryNode(i) + fmt.Sprintf("/pod/pod%d", i)
		if len(leaf.URIs) != 1 || leaf.URIs[0].String() != uri || !key.PublicKey.Equal(leaf.PublicKey) {
			t.Fatal("issued identity must bind the enrolled pod and CSR key, ignoring requested SANs")
		}

		nodeTLS := tlsConfig.Clone()
		nodeTLS.Certificates = []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}
		nodeClient := newClient(nodeTLS)
		request(nodeClient, "GET", controlURL, "", "bad", nil, 400)
		data = request(nodeClient, "GET", controlURL, "", boot, nil, 200)

		var desired pb.DesiredState
		if err := proto.Unmarshal(data, &desired); err != nil {
			t.Fatal(err)
		}

		if desired.Configuration.GetSnapshot() == nil {
			t.Fatal("missing desired snapshot")
		}

		raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(desired.Configuration.GetSnapshot())

		digest := sha256.Sum256(raw)
		if hex.EncodeToString(desired.Node) != gantryNode(i) || !bytes.Equal(desired.Universe, bytes.Repeat([]byte{1}, 32)) || hex.EncodeToString(desired.Incarnation) != boot || desired.PodUid != fmt.Sprintf("pod%d", i) || desired.Profile != 1 || desired.Revision != 1 || !bytes.Equal(desired.SnapshotDigest, digest[:]) || desired.Cursor != hex.EncodeToString(digest[:]) {
			t.Fatal("desired state does not bind the authenticated process and snapshot")
		}

		request(nodeClient, "POST", proofURL, "", boot, nil, 204)

		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)

		r, err := http.NewRequestWithContext(ctx, "GET", controlURL, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		r.Header.Set("X-Racer-Boot", boot)
		r.Header.Set("X-Racer-Cursor", desired.Cursor)
		resp, err := nodeClient.Do(r)

		cancel()

		if resp != nil {
			resp.Body.Close()
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unchanged cursor must hold until cancellation: %v", err)
		}
	}

	for _, name := range []string{"untrusted-root", "wrong-server-name"} {
		t.Run(name, func(t *testing.T) {
			config := tlsConfig.Clone()
			if name == "untrusted-root" {
				config.RootCAs = x509.NewCertPool()
			} else {
				config.ServerName = "wrong.invalid"
			}

			resp, err := newClient(config).Get(controlURL)
			if resp != nil {
				resp.Body.Close()
			}

			if err == nil {
				t.Fatal("control TLS accepted invalid server trust")
			}
		})
	}
}
