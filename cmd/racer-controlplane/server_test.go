// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
)

func fixture() *pb.Snapshot {
	return &pb.Snapshot{
		Universe: bytes.Repeat([]byte{1}, 32), Node: bytes.Repeat([]byte{2}, 32), Revision: 1,
		Volumes: []*pb.Volume{{Id: "v1", Listen: "127.0.0.1:8081", OriginAddress: "127.0.0.1:8082", OriginIdentity: "test/origin:8082", PeerEndpoints: &pb.VolumePeerEndpoints{}}},
	}
}

func target(snapshot *pb.Snapshot) string {
	return "/" + hex.EncodeToString(snapshot.Universe) + "/" + hex.EncodeToString(snapshot.Node)
}

// Test adapter for inspecting the serialized publication cache. The production
// HTTP interface is the authenticated, phased control endpoint tested in rollout_test.go.
func handler(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{universe}/{node}", func(w http.ResponseWriter, r *http.Request) {
		u, e1 := hex.DecodeString(r.PathValue("universe"))

		n, e2 := hex.DecodeString(r.PathValue("node"))
		if e1 != nil || e2 != nil || len(u) != 32 || len(n) != 32 {
			w.WriteHeader(400)
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		e, err := s.current(recipient{[32]byte(u), [32]byte(n)})
		if err != nil {
			w.WriteHeader(500)
			return
		}

		if e == nil {
			w.WriteHeader(404)
			return
		}

		w.Header().Set("ETag", e.etag)
		w.Header().Set("Content-Length", fmt.Sprint(len(e.body)))
		w.Write(e.body)
	})

	return mux
}

func installTestGeneration(t *testing.T, s *Server, g *generation) *pb.Snapshot {
	t.Helper()

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	return index.snapshot(g.Nodes["node-000000"].ID)
}

func get(h http.Handler, path, etag, prefer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("If-None-Match", etag)
	r.Header.Set("Prefer", prefer)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

func checkSnapshot(t *testing.T, w *httptest.ResponseRecorder, want *pb.Snapshot) {
	t.Helper()

	var config pb.Configuration
	if w.Code != http.StatusOK || proto.Unmarshal(w.Body.Bytes(), &config) != nil || !proto.Equal(config.GetSnapshot(), want) {
		t.Fatalf("unexpected configuration: status=%d, config=%v", w.Code, &config)
	}
}

func TestLazyGenerationEvictionRotationAndRemoval(t *testing.T) {
	g := testGeneration(8, 2)

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{signer: testSigner(t, 7)}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	want := index.snapshot(g.Nodes["node-000000"].ID)
	checkSigned(t, get(handler(s), target(want), "", ""), s.signer, want)
	s.mu.Lock()
	for s.source.lru.Len() != 0 {
		s.source.remove(s.source.lru.Back())
	}
	s.mu.Unlock()

	key := testSigner(t, 8)
	if err := s.rotate(key); err != nil {
		t.Fatal(err)
	}

	checkSigned(t, get(handler(s), target(want), "", ""), key, want)

	next := *g
	next.Revision++
	next.Owners, next.Volume = nil, nil

	index, err = indexGeneration(&next)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	want = index.snapshot(g.Nodes["node-000000"].ID)
	checkSigned(t, get(handler(s), target(want), "", ""), key, want)

	if len(want.Volumes) != 0 || want.Revision != 2 {
		t.Fatal("invalid removal")
	}

	if err := s.install(&topologyIndex{g: g}); err == nil {
		t.Fatal("accepted rollback")
	}
}
