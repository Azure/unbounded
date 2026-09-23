// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/racer/pki"
)

func issueTLSFixture(t testing.TB, manager *pki.Manager, id pki.Identity, probe bool) (pki.IssuedCertificate, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	request := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})

	var issued pki.IssuedCertificate
	if probe {
		issued, err = manager.IssueProbe(t.Context(), request, id)
	} else {
		issued, err = manager.Issue(t.Context(), request, id)
	}

	if err != nil {
		t.Fatal(err)
	}

	return issued, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestTrustProofRequiresFreshPendingRootTLS(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).Build()

	manager, err := pki.New(kube, "system", pki.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.AcquireLeadership(t.Context(), "leader"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}

	id := pki.Identity{Kind: pki.Node, Universe: strings.Repeat("a", 64), Node: strings.Repeat("b", 64), PodUID: "pod-uid", BootID: strings.Repeat("c", 64)}

	node, key := issueTLSFixture(t, manager, id, false)
	if err := manager.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	cp, cpKey := issueTLSFixture(t, manager, pki.Identity{Kind: pki.ControlPlane, PodUID: "controller", BootID: "boot"}, true)

	hot := pki.NewHotTLS()
	if err := hot.Update(cp.Bundle.JSON(), cp.CertificatePEM, cpKey); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- serveTrustProof(ctx, listener, hot, manager) }()

	defer func() {
		cancel()

		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	pair, err := tls.X509KeyPair(node.CertificatePEM, key)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(cp.Bundle.Certificates))

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}, ServerName: "racer-controlplane.system.svc"}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}

	request := func(digest, boot, issuer string) int {
		req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+listener.Addr().String()+"/v3/proof", nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("X-Racer-Boot", boot)
		req.Header.Set("X-Racer-Trust-Generation", strconv.FormatUint(cp.Bundle.Generation, 10))
		req.Header.Set("X-Racer-Trust-Digest", digest)
		req.Header.Set("X-Racer-Certificate-Issuer", issuer)
		req.Header.Set("X-Racer-Old-Connections", "0")

		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()

		return response.StatusCode
	}
	if status := request(cp.Bundle.Digest(), id.BootID, node.RootDigest); status != http.StatusNoContent {
		t.Fatalf("old enrolled client proving pending server root: %d", status)
	}

	if status := request(strings.Repeat("0", 64), id.BootID, node.RootDigest); status != http.StatusConflict {
		t.Fatalf("wrong exact digest accepted: %d", status)
	}

	if status := request(cp.Bundle.Digest(), strings.Repeat("d", 64), node.RootDigest); status != http.StatusConflict {
		t.Fatalf("different boot reused leaf: %d", status)
	}

	if status := request(cp.Bundle.Digest(), id.BootID, cp.RootDigest); status != http.StatusBadRequest {
		t.Fatalf("self-reported issuer accepted: %d", status)
	}
}
