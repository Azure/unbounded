// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
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
)

func TestEnrollmentAndControl(t *testing.T) {
	f, err := newFixture(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	r := registration{Universe: strings.Repeat("01", 32), Node: strings.Repeat("02", 32), PodUID: "probe-pod", Token: "test-token", Config: filepath.Join(f.dir, "config.json")}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(f.dir, r.Node+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot := &pb.Snapshot{Universe: bytes.Repeat([]byte{1}, 32), Node: bytes.Repeat([]byte{2}, 32), Revision: 1, Epoch: 1}
	// Exceed net/http's implicit Content-Length buffer, as the Go physical-owner
	// exports do. Chunked control responses are rejected by the Rust subscriber.
	for i := range 1000 {
		snapshot.Volumes = append(snapshot.Volumes, &pb.Volume{Id: strconv.Itoa(i), PeerEndpoints: &pb.VolumePeerEndpoints{}})
	}

	config := &pb.Configuration{Contents: &pb.Configuration_Snapshot{Snapshot: snapshot}}
	writeConfig := func() {
		t.Helper()

		data, err := protojson.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(r.Config, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig()

	server := httptest.NewUnstartedServer(f.handler())
	server.TLS = f.tls

	server.StartTLS()
	defer server.Close()

	roots := x509.NewCertPool()
	roots.AddCert(f.root)

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost"}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	request := func(client *http.Client, method, path string, data []byte, headers map[string]string) (int, []byte, int64) {
		t.Helper()

		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("X-Racer-Boot", strings.Repeat("03", 32))

		for name, value := range headers {
			req.Header.Set(name, value)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		return resp.StatusCode, body, resp.ContentLength
	}

	var certificate []byte

	for _, tc := range []struct {
		name, token, uri, boot string
		want                   int
	}{
		{"wrong token", "wrong", r.uri(), strings.Repeat("03", 32), 403},
		{"wrong CSR identity", r.Token, r.uri() + "-wrong", strings.Repeat("03", 32), 403},
		{"invalid boot", r.Token, r.uri(), "invalid", 403},
		{"valid enrollment", r.Token, r.uri(), strings.Repeat("03", 32), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri, err := url.Parse(tc.uri)
			if err != nil {
				t.Fatal(err)
			}

			csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{uri}}, key)
			if err != nil {
				t.Fatal(err)
			}

			body, err := json.Marshal(map[string]string{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), "pod_namespace": "probe", "pod_name": r.Node, "expected_universe": r.Universe, "expected_node": r.Node})
			if err != nil {
				t.Fatal(err)
			}

			status, data, _ := request(client, "POST", "/v3/enroll", body, map[string]string{"Authorization": "Bearer " + tc.token, "X-Racer-Boot": tc.boot})
			if status != tc.want {
				t.Fatalf("status %d: %s", status, data)
			}

			if status == 200 {
				var reply struct {
					Certificate string
					Generation  uint64
					Issuer      string
				}
				if err := json.Unmarshal(data, &reply); err != nil || reply.Generation != 1 || reply.Issuer != f.issuer {
					t.Fatalf("enrollment reply: %+v, %v", reply, err)
				}

				block, _ := pem.Decode([]byte(reply.Certificate))
				certificate = block.Bytes
			}
		})
	}

	path := "/v4/config"
	for _, endpoint := range []struct{ method, path string }{{"GET", path}, {"POST", "/v3/proof"}} {
		status, _, _ := request(client, endpoint.method, endpoint.path, nil, nil)
		if status != http.StatusForbidden {
			t.Fatalf("unauthenticated %s: %d", endpoint.path, status)
		}
	}

	mtls := transport.Clone()

	mtls.TLSClientConfig.Certificates = []tls.Certificate{{Certificate: [][]byte{certificate}, PrivateKey: key}}
	defer mtls.CloseIdleConnections()

	client = &http.Client{Transport: mtls, Timeout: 5 * time.Second}
	for _, path := range []string{"/v4/config?universe=" + strings.Repeat("04", 32), "/v4/config?node=" + strings.Repeat("04", 32)} {
		status, _, _ := request(client, "GET", path, nil, nil)
		if status != http.StatusForbidden {
			t.Fatalf("cross-identity control: %d", status)
		}
	}

	if status, _, _ := request(client, "GET", path, nil, map[string]string{"X-Racer-Boot": strings.Repeat("05", 32)}); status != http.StatusForbidden {
		t.Fatalf("signed boot mismatch accepted: %d", status)
	}

	checkCommand := func(headers map[string]string) {
		t.Helper()

		status, body, length := request(client, "GET", path, nil, headers)

		var command pb.DesiredState
		if status != 200 || length != int64(len(body)) || proto.Unmarshal(body, &command) != nil {
			t.Fatalf("control response status=%d length=%d body=%d", status, length, len(body))
		}

		wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}

		digest := sha256.Sum256(wire)
		if !proto.Equal(command.Configuration, config) || !bytes.Equal(command.SnapshotDigest, digest[:]) || command.Revision != snapshot.Revision || command.Cursor != hex.EncodeToString(digest[:]) || command.PodUid != r.PodUID || command.Profile != 1 || !bytes.Equal(command.Incarnation, bytes.Repeat([]byte{3}, 32)) {
			t.Fatal("desired state lost snapshot, digest, cursor, or enrolled identity")
		}
	}
	checkCommand(nil)

	wire, err := proto.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256(wire)
	headers := map[string]string{"X-Racer-Applied-Digest": hex.EncodeToString(digest[:]), "X-Racer-Applied-Revision": "0", "X-Racer-Local-State": "failed"}
	checkCommand(headers)
	headers["X-Racer-Cursor"] = hex.EncodeToString(digest[:])

	snapshot.Revision = 2

	writeConfig()
	checkCommand(headers)

	status, _, _ := request(client, "POST", "/v3/proof", nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("authenticated proof: %d", status)
	}

	if _, err := f.registration("../../config"); err == nil {
		t.Fatal("unsafe registration path accepted")
	}
}
