// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer"
)

func controlTLS(req *http.Request, pod string) {
	u := &url.URL{Scheme: "spiffe", Host: "racer", Path: "/universe/" + req.PathValue("universe") + "/node/" + req.PathValue("node") + "/pod/" + pod}
	leaf := &x509.Certificate{URIs: []*url.URL{u}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
}

func configurationSnapshot(tb testing.TB, config *pb.Configuration) []byte {
	tb.Helper()

	if config.GetSnapshot() == nil {
		tb.Fatal("missing plain snapshot")
	}

	raw, err := marshalSnapshot(config.GetSnapshot())
	if err != nil {
		tb.Fatal(err)
	}

	return raw
}

func TestCoordinationTLSFixture(t *testing.T) {
	f := newCoordinationFixture(t, nil)

	storage := newStorageTest(t, f.api, f.s)
	if _, err := storage.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}}); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/{universe}/{node}", f.s.control)

	server, pki := coordinationServer(t, mux)
	defer server.Close()

	cert, key := pki.leaf(t, "spiffe://racer/universe/"+identity("universe", "default")+"/node/"+f.node+"/pod/pod-uid", "")

	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(pki.root)

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{pair}}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v3/"+identity("universe", "default")+"/"+f.node, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
	req.Header.Set("X-Racer-Profile", "1")
	req.Header.Set("X-Racer-Phase", "0")
	req.Header.Set("X-Racer-Storage-Policy", "1")

	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	var command pb.ControlCommand
	if response.StatusCode != http.StatusOK || proto.Unmarshal(body, &command) != nil || command.PodUid != "pod-uid" || command.Configuration.GetSnapshot() == nil {
		t.Fatalf("TLS control response %d: %s", response.StatusCode, body)
	}

	if policy := command.StoragePolicy; policy == nil || len(policy.Identity) != 32 || policy.Version != 1 || policy.DesiredBytes != uint64(racer.DefaultCacheSizeBytes) {
		t.Fatalf("TLS storage policy missing or invalid: %v", policy)
	}
}

func TestPlainPublication(t *testing.T) {
	s := &Server{}
	g := testGeneration(8, 2)
	first := installTestGeneration(t, s, g)

	second := s.source.topologies[[32]byte(first.Universe)].snapshot(g.Nodes["node-000001"].ID)
	for _, snapshot := range []*pb.Snapshot{first, second} {
		w := get(handler(s), target(snapshot), "", "")
		checkSnapshot(t, w, snapshot)
		installTestGeneration(t, s, g)

		if next := get(handler(s), target(snapshot), "", ""); next.Header().Get("ETag") != w.Header().Get("ETag") {
			t.Fatal("identical publication changed ETag")
		}
	}

	conflict := *g

	conflict.Volume = nil
	if err := s.install(&topologyIndex{g: &conflict}); err == nil {
		t.Fatal("conflicting revision accepted")
	}

	updated := *g
	updated.Revision++

	want := installTestGeneration(t, s, &updated)
	if err := s.install(&topologyIndex{g: g}); err == nil {
		t.Fatal("rollback accepted")
	}

	checkSnapshot(t, get(handler(s), target(want), "", ""), want)
}

func TestConcurrentPublication(t *testing.T) {
	s := &Server{}
	g := testGeneration(8, 2)
	snapshot := installTestGeneration(t, s, g)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		for range 50 {
			w := get(handler(s), target(snapshot), "", "")

			var config pb.Configuration
			if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &config) != nil || config.GetSnapshot() == nil {
				t.Error("invalid concurrent publication")
			}
		}
	}()

	for i := uint64(2); i <= 50; i++ {
		updated := *g
		updated.Revision = i
		next := installTestGeneration(t, s, &updated)
		checkSnapshot(t, get(handler(s), target(next), "", ""), next)
	}

	wg.Wait()
}

func TestLargeConfigurationDelivery(t *testing.T) {
	s := &Server{}
	g := testGeneration(8, 2)
	node := g.Nodes["node-000000"]
	node.Fabric = strings.Repeat("x", 5*1024*1024)
	g.Nodes["node-000000"] = node
	snapshot := installTestGeneration(t, s, g)

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

	var config pb.Configuration
	if err := proto.Unmarshal(body, &config); err != nil || !proto.Equal(config.GetSnapshot(), snapshot) {
		t.Fatalf("large configuration round trip failed: %v", err)
	}
}
