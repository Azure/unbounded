// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"

	pb "github.com/Azure/unbounded/api/racer"
)

func TestGenerateKey(t *testing.T) {
	if dir := os.Getenv("RACER_TEST_GENERATE_KEY"); dir != "" {
		flag.CommandLine = flag.NewFlagSet("keygen", flag.ExitOnError)
		os.Args = []string{"racer-controlplane", "-generate-key", dir}

		main()
		os.Exit(0)
	}

	root := t.TempDir()

	var previous []byte

	for _, name := range []string{"control", "peer"} {
		dir := filepath.Join(root, name)
		cmd := exec.Command(os.Args[0], "-test.run=^TestGenerateKey$")

		cmd.Env = append(os.Environ(), "RACER_TEST_GENERATE_KEY="+dir, "KUBECONFIG=/missing", "RACER_SIGNING_KEY=/missing")
		if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
			t.Fatalf("keygen must exit silently without Kubernetes or existing keys: %s %v", output, err)
		}

		seed, err := os.ReadFile(filepath.Join(dir, "seed"))
		if err != nil || len(seed) != ed25519.SeedSize || bytes.Equal(seed, previous) {
			t.Fatalf("invalid or reused seed: %v", err)
		}

		previous = seed

		public, err := os.ReadFile(filepath.Join(dir, "public"))
		if err != nil || !bytes.Equal(public, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)) {
			t.Fatalf("public key does not match seed: %v", err)
		}

		for path, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "seed"): 0o600, filepath.Join(dir, "public"): 0o644} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() & ^mode != 0 {
				t.Fatalf("unsafe permissions on %s: %v", path, err)
			}
		}

		key, err := readSigner(bytes.NewReader(seed))
		if err != nil {
			t.Fatal(err)
		}

		s := &Server{signer: key}
		snapshot := installTestGeneration(t, s, testGeneration(8, 2))
		checkSigned(t, get(handler(s), target(snapshot), "", ""), key, snapshot)

		if err := generateKey(dir); err == nil {
			t.Fatal("overwrote existing directory")
		}

		after, _ := os.ReadFile(filepath.Join(dir, "seed"))
		if !bytes.Equal(after, seed) {
			t.Fatal("existing seed changed")
		}
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "control"), link); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{link, filepath.Join(root, "control", "seed"), filepath.Join(root, "missing", "key"), ""} {
		if err := generateKey(path); err == nil {
			t.Fatalf("accepted invalid/existing destination %q", path)
		}
	}
}

func TestSigningDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "key")
	if err := generateKey(dir); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "short"), []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	// Preserve the parent environment while exercising the unset case.
	t.Setenv("RACER_SIGNING_KEY", "")

	for _, allow := range []string{"", "0", "true", "1"} {
		t.Setenv("RACER_ALLOW_UNSIGNED", allow)
		os.Unsetenv("RACER_SIGNING_KEY")

		c := signingTestClient(t)

		key, err := signerFromEnv(context.Background(), c, "state")
		if err != nil || key == nil || !managedSigningEnabled() {
			t.Fatalf("missing key with opt-out %q: %v", allow, err)
		}

		var secrets corev1.SecretList
		if err := c.List(context.Background(), &secrets); err != nil {
			t.Fatal(err)
		}

		want := 2
		if len(secrets.Items) != want {
			t.Fatalf("mode provisioned %d Secrets, want %d", len(secrets.Items), want)
		}

		for _, path := range []string{"", filepath.Join(dir, "missing"), filepath.Join(dir, "short"), filepath.Join(dir, "seed")} {
			t.Setenv("RACER_SIGNING_KEY", path)

			if _, err := signerFromEnv(context.Background(), nil, "state"); err == nil {
				t.Fatalf("removed file mode accepted: %q", path)
			}
		}
	}
}

func testSigner(t *testing.T, seed byte) *signer {
	t.Helper()

	key, err := readSigner(bytes.NewReader(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal(err)
	}

	return key
}

// Verify independently from the production signer, using the dataplane's
// canonical layout and the exact bytes received in the envelope.
func verifyConfig(body []byte, key *signer) (*pb.Snapshot, error) {
	var envelope pb.Configuration
	if err := proto.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}

	signed := envelope.GetSigned()
	if signed == nil || len(signed.Signature) != 96 {
		return nil, fmt.Errorf("missing or malformed signature")
	}

	public := key.key.Public().(ed25519.PublicKey)

	id := sha256.Sum256(append([]byte("racer/public-key/v2"), public...))
	if !bytes.Equal(signed.Signature[:32], id[:]) {
		return nil, fmt.Errorf("unknown key")
	}

	var message bytes.Buffer
	message.WriteString("RACERSIG2")

	for _, field := range [][]byte{[]byte("racer/config/v2"), signed.Snapshot} {
		if err := binary.Write(&message, binary.BigEndian, uint64(len(field))); err != nil {
			return nil, err
		}

		message.Write(field)
	}

	if !ed25519.Verify(public, message.Bytes(), signed.Signature[32:]) {
		return nil, fmt.Errorf("invalid signature")
	}

	var snapshot pb.Snapshot
	if err := proto.Unmarshal(signed.Snapshot, &snapshot); err != nil {
		return nil, err
	}

	return &snapshot, nil
}

func checkSigned(t *testing.T, w *httptest.ResponseRecorder, key *signer, want *pb.Snapshot) {
	t.Helper()

	got, err := verifyConfig(w.Body.Bytes(), key)
	if w.Code != 200 || err != nil || !proto.Equal(got, want) {
		t.Fatalf("signed response: status=%d, error=%v", w.Code, err)
	}

	if w.Header().Get("ETag") != fmt.Sprintf(`"%x"`, sha256.Sum256(w.Body.Bytes())) {
		t.Fatal("ETag does not describe envelope")
	}
}

func TestDataplaneSigningVector(t *testing.T) {
	// Generated and verified with racer-dataplane/src/signing.rs:
	// Keys::new(Some([7;32]), vec![public]).sign(b"racer/config/v2", &[snapshot]).
	const (
		public    = "ea4a6c63e29c520abef5507b132ec5f9954776aebebe7b92421eea691446d22c"
		signature = "3bf836d483dd4e305b21b727ed1a2d01df7de0c36cdd98ddcec5f5cb8220998c50c1acf505f9ec8be74dd86901ddc5bb1075ac875216ab1eb2275032fd8ee53164a4561ad710951782a0dc4ae814b5cc6dd4c1cf8f8034de874583f5dc16ea0e"
	)

	key := testSigner(t, 7)
	if hex.EncodeToString(key.key[32:]) != public {
		t.Fatal("public key differs from dataplane")
	}

	snapshot := fixture()
	snapshot.Volumes = nil

	raw, err := marshalSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	body, err := configuration(raw, key)
	if err != nil {
		t.Fatal(err)
	}

	var envelope pb.Configuration
	if err := proto.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}

	if hex.EncodeToString(envelope.GetSigned().Signature) != signature {
		t.Fatal("signature differs from dataplane vector")
	}

	for _, mutate := range []func(*pb.SignedSnapshot){
		func(s *pb.SignedSnapshot) { s.Snapshot[4] ^= 1 },
		func(s *pb.SignedSnapshot) { s.Signature[0] ^= 1 },
		func(s *pb.SignedSnapshot) { s.Signature[50] ^= 1 },
		func(s *pb.SignedSnapshot) { s.Signature = s.Signature[:95] },
	} {
		bad := proto.Clone(&envelope).(*pb.Configuration)
		mutate(bad.GetSigned())

		tampered, err := proto.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := verifyConfig(tampered, key); err == nil {
			t.Fatal("tampered envelope accepted")
		}
	}

	if _, err := verifyConfig(body, testSigner(t, 8)); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestSignedRotationAndPublication(t *testing.T) {
	old, next := testSigner(t, 7), testSigner(t, 8)
	s := &Server{signer: old}
	h := handler(s)
	g := testGeneration(8, 2)
	first := installTestGeneration(t, s, g)

	second := s.source.topologies[[32]byte(first.Universe)].snapshot(g.Nodes["node-000001"].ID)
	for _, snapshot := range []*pb.Snapshot{first, second} {
		w := get(h, target(snapshot), "", "")
		checkSigned(t, w, old, snapshot)
	}

	if err := s.rotate(next); err != nil {
		t.Fatal(err)
	}

	for _, snapshot := range []*pb.Snapshot{first, second} {
		checkSigned(t, get(h, target(snapshot), "", ""), next, snapshot)
		w := get(h, target(snapshot), "", "")
		etag := w.Header().Get("ETag")

		installTestGeneration(t, s, g)

		if err := s.rotate(next); err != nil {
			t.Fatal(err)
		}

		if w := get(h, target(snapshot), "", ""); w.Header().Get("ETag") != etag {
			t.Fatal("identical publication changed ETag")
		}
	}

	conflict := *g

	conflict.Volume = nil
	if err := s.install(&topologyIndex{g: &conflict}); err == nil {
		t.Fatal("conflicting revision accepted after rotation")
	}

	updated := *g
	updated.Revision++

	want := installTestGeneration(t, s, &updated)
	if err := s.install(&topologyIndex{g: g}); err == nil {
		t.Fatal("rollback accepted after rotation")
	}

	checkSigned(t, get(h, target(want), "", ""), next, want)
}

func TestConcurrentRotation(t *testing.T) {
	keys := []*signer{testSigner(t, 7), testSigner(t, 8)}
	s := &Server{signer: keys[0]}
	g := testGeneration(8, 2)
	snapshot := installTestGeneration(t, s, g)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		for i := 0; i < 50; i++ {
			if err := s.rotate(keys[i%2]); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()

		for i := uint64(2); i <= 50; i++ {
			updated := *g
			updated.Revision = i
			next := installTestGeneration(t, s, &updated)

			w := get(handler(s), target(next), "", "")
			if _, err := verifyConfig(w.Body.Bytes(), keys[0]); err != nil {
				if _, err := verifyConfig(w.Body.Bytes(), keys[1]); err != nil {
					t.Error(err)
				}
			}
		}
	}()

	wg.Wait()

	if err := s.rotate(keys[1]); err != nil {
		t.Fatal(err)
	}

	snapshot = s.source.topologies[[32]byte(snapshot.Universe)].snapshot(g.Nodes["node-000000"].ID)
	checkSigned(t, get(handler(s), target(snapshot), "", ""), keys[1], snapshot)
}

func TestLargeConfigurationDelivery(t *testing.T) {
	for _, signed := range []bool{false, true} {
		t.Run(fmt.Sprintf("signed=%t", signed), func(t *testing.T) {
			s := new(Server)
			if signed {
				s.signer = testSigner(t, 7)
			}

			g := testGeneration(8, 2)
			node := g.Nodes["node-000000"]
			node.Fabric = strings.Repeat("x", 5*1024*1024)
			g.Nodes["node-000000"] = node

			snapshot := installTestGeneration(t, s, g)
			if signed {
				if err := s.rotate(testSigner(t, 8)); err != nil {
					t.Fatal(err)
				}
			}

			server := httptest.NewServer(handler(s))
			defer server.Close()

			response, err := server.Client().Get(server.URL + target(snapshot))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()

			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}

			if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(body)) || len(body) <= 4*1024*1024 {
				t.Fatal("large response truncated or rejected")
			}

			var got *pb.Snapshot
			if signed {
				got, err = verifyConfig(body, s.signer)
			} else {
				var envelope pb.Configuration

				err = proto.Unmarshal(body, &envelope)
				got = envelope.GetSnapshot()
			}

			if err != nil || !proto.Equal(got, snapshot) {
				t.Fatalf("large configuration round trip failed: %v", err)
			}
		})
	}
}
