// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// controlfixture supplies loopback enrollment and mTLS control for daemon probes.
package main

import (
	"bytes"
	"context"
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
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
)

type registration struct {
	Universe string `json:"universe"`
	Node     string `json:"node"`
	PodUID   string `json:"podUID"`
	Token    string `json:"token"`
	Config   string `json:"config"`
}

func (r registration) uri() string {
	return "spiffe://racer/universe/" + r.Universe + "/node/" + r.Node + "/pod/" + r.PodUID
}

type fixture struct {
	dir    string
	root   *x509.Certificate
	key    *ecdsa.PrivateKey
	bundle []byte
	issuer string
	tls    *tls.Config
}

func serial() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }

func newFixture(dir string) (*fixture, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	number, err := serial()
	if err != nil {
		return nil, err
	}

	root := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: "Racer loopback probe CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	root, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	digest := sha256.Sum256(der)
	f := &fixture{dir: dir, root: root, key: key, issuer: hex.EncodeToString(digest[:])}

	f.bundle, err = json.Marshal(map[string]any{"version": 1, "generation": 1, "active": f.issuer, "certificates": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))})
	if err != nil {
		return nil, err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	leaf, err := f.issue(&serverKey.PublicKey, "spiffe://racer/controlplane", []string{"localhost"})
	if err != nil {
		return nil, err
	}

	roots := x509.NewCertPool()
	roots.AddCert(root)
	f.tls = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots, Certificates: []tls.Certificate{{Certificate: [][]byte{leaf, root.Raw}, PrivateKey: serverKey}}}

	return f, nil
}

func (f *fixture) issue(key any, identity string, dns []string) ([]byte, error) {
	number, err := serial()
	if err != nil {
		return nil, err
	}

	uri, err := url.Parse(identity)
	if err != nil {
		return nil, err
	}

	leaf := &x509.Certificate{SerialNumber: number, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(30 * time.Minute), URIs: []*url.URL{uri}, DNSNames: dns, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}

	return x509.CreateCertificate(rand.Reader, leaf, f.root, key, f.key)
}

func (f *fixture) registration(node string) (registration, error) {
	var r registration
	if b, err := hex.DecodeString(node); err != nil || len(b) != 32 || node != hex.EncodeToString(b) {
		return r, errors.New("invalid node")
	}

	data, err := os.ReadFile(filepath.Join(f.dir, node+".json"))
	if err != nil {
		return r, err
	}

	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}

	if r.Node != node || r.PodUID == "" || r.Token == "" {
		return r, errors.New("incomplete registration")
	}

	return r, nil
}

func boot(req *http.Request) ([]byte, error) {
	value, err := hex.DecodeString(req.Header.Get("X-Racer-Boot"))
	if err != nil || len(value) != 32 {
		return nil, errors.New("invalid boot")
	}

	return value, nil
}

func (f *fixture) enroll(w http.ResponseWriter, req *http.Request) {
	var body struct {
		CSR       string `json:"csr"`
		Namespace string `json:"pod_namespace"`
		Pod       string `json:"pod_name"`
		Universe  string `json:"expected_universe"`
		Node      string `json:"expected_node"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16384)).Decode(&body); err != nil {
		http.Error(w, "invalid enrollment", http.StatusBadRequest)
		return
	}

	r, err := f.registration(body.Pod)

	_, bootErr := boot(req)
	if err != nil || bootErr != nil || body.Namespace != "probe" || body.Universe != r.Universe || body.Node != r.Node || req.Header.Get("Authorization") != "Bearer "+r.Token {
		http.Error(w, "unauthorized enrollment", http.StatusForbidden)
		return
	}

	block, _ := pem.Decode([]byte(body.CSR))
	if block == nil {
		http.Error(w, "missing CSR", http.StatusBadRequest)
		return
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil || len(csr.URIs) != 1 || csr.URIs[0].String() != r.uri() || len(csr.DNSNames) != 0 || len(csr.IPAddresses) != 0 {
		http.Error(w, "invalid CSR identity", http.StatusForbidden)
		return
	}

	leaf, err := f.issue(csr.PublicKey, r.uri(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.root.Raw})...)

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(map[string]any{"certificate": string(chain), "generation": 1, "issuer": f.issuer}); err != nil {
		log.Print(err)
	}
}

func (f *fixture) authenticated(req *http.Request) (registration, error) {
	if req.TLS == nil || len(req.TLS.VerifiedChains) == 0 {
		return registration{}, errors.New("client certificate required")
	}

	leaf := req.TLS.VerifiedChains[0][0]
	if len(leaf.URIs) != 1 {
		return registration{}, errors.New("invalid client identity")
	}

	parts := bytes.Split([]byte(leaf.URIs[0].Path), []byte("/"))
	if len(parts) != 7 {
		return registration{}, errors.New("invalid client URI")
	}

	r, err := f.registration(string(parts[4]))
	if err != nil || leaf.URIs[0].String() != r.uri() {
		return registration{}, errors.New("unregistered client identity")
	}

	return r, nil
}

func (f *fixture) control(w http.ResponseWriter, req *http.Request) {
	r, err := f.authenticated(req)

	incarnation, bootErr := boot(req)
	if err != nil || bootErr != nil || (req.URL.Query().Has("universe") && req.URL.Query().Get("universe") != r.Universe) || (req.URL.Query().Has("node") && req.URL.Query().Get("node") != r.Node) {
		http.Error(w, "unauthorized control", http.StatusForbidden)
		return
	}

	deadline := time.NewTimer(28 * time.Second)
	defer deadline.Stop()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		data, err := os.ReadFile(r.Config)

		var config pb.Configuration
		if err != nil || protojson.Unmarshal(data, &config) != nil || config.GetSnapshot() == nil {
			http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
			return
		}

		snapshot := config.GetSnapshot()
		if hex.EncodeToString(snapshot.Universe) != r.Universe || hex.EncodeToString(snapshot.Node) != r.Node {
			http.Error(w, "snapshot identity mismatch", http.StatusConflict)
			return
		}

		wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(snapshot)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		digest := sha256.Sum256(wire)

		cursor := hex.EncodeToString(digest[:])
		if req.Header.Get("X-Racer-Cursor") == cursor {
			select {
			case <-req.Context().Done():
				return
			case <-deadline.C:
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusNoContent)

				return
			case <-tick.C:
				continue
			}
		}

		command, err := proto.Marshal(&pb.DesiredState{Universe: snapshot.Universe, Node: snapshot.Node, Incarnation: incarnation, SnapshotDigest: digest[:], Revision: snapshot.Revision, Configuration: &config, Profile: 1, PodUid: r.PodUID, Cursor: cursor})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Length", strconv.Itoa(len(command)))

		if _, err := w.Write(command); err != nil {
			log.Print(err)
		}

		return
	}
}

func (f *fixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/enroll", f.enroll)
	mux.HandleFunc("GET /v4/config", f.control)
	mux.HandleFunc("POST /v3/proof", func(w http.ResponseWriter, r *http.Request) {
		if _, err := f.authenticated(r); err != nil {
			http.Error(w, "unauthorized proof", http.StatusForbidden)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func run(dir string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	f, err := newFixture(dir)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Print(err)
		}
	}()

	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), f.bundle, 0o600); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "url.next"), []byte("https://"+listener.Addr().String()), 0o600); err != nil {
		return err
	}

	if err := os.Rename(filepath.Join(dir, "url.next"), filepath.Join(dir, "url")); err != nil {
		return err
	}

	server := &http.Server{Handler: f.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()

		if err := server.Close(); err != nil {
			log.Print(err)
		}
	}()

	if err := server.Serve(tls.NewListener(listener, f.tls)); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func main() {
	dir := flag.String("dir", "", "private probe credential/registration directory")

	flag.Parse()

	if *dir == "" {
		log.Fatal("--dir is required")
	}

	if err := run(*dir); err != nil {
		log.Fatal(fmt.Errorf("probe control fixture: %w", err))
	}
}
