// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServingReadinessRequiresConfiguredHostname(t *testing.T) {
	for _, names := range [][]string{nil, {"other-service.racer.svc"}, {"racer-controller.racer.svc"}, {"*.racer.svc"}} {
		t.Run("names="+strings.Join(names, ","), func(t *testing.T) {
			f := newServingFixture(t)
			certificate := servingTestCertificate(t, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), nil, false)
			leaf := *certificate.Leaf
			leaf.DNSNames = names
			// A matching CommonName alone must not bypass SAN validation.
			leaf.Subject.CommonName = f.a.Server.Config.ReplicationServerName

			der, err := x509.CreateCertificate(rand.Reader, &leaf, &leaf, leaf.PublicKey, certificate.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}

			certificate = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: certificate.PrivateKey}
			dir := t.TempDir()
			writeServingTestPair(t, dir, certificate)

			r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
			if err != nil {
				t.Fatal(err)
			}

			f.a.Server.servingCertificate.Store(r)

			wantReady := len(names) != 0 && names[0] != "other-service.racer.svc"
			if ready := f.a.Server.Ready(nil) == nil; ready != wantReady {
				t.Fatalf("ready=%v want=%v", ready, wantReady)
			}
			// Reloading a correctly named certificate repairs readiness without
			// restart; mismatched names do not corrupt issuer trust/readiness.
			writeServingTestPair(t, dir, f.serverCertificate)

			if err := r.reload(); err != nil {
				t.Fatal(err)
			}

			if err := f.a.Server.Ready(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
