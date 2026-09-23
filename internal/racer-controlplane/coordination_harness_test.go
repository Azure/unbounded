// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kubetesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
)

// Coordination barriers, command validation and mutual-TLS heartbeats.
type coordinationPKI struct {
	root *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func (p *coordinationPKI) leaf(t *testing.T, uri, dns string) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}

	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{u}}
	if dns != "" {
		leaf.DNSNames = []string{dns}
	}

	der, err := x509.CreateCertificate(rand.Reader, leaf, p.root, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}

	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}

func coordinationServer(t *testing.T, handler http.Handler) (*httptest.Server, *coordinationPKI) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "coordination root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	p := &coordinationPKI{root: root, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	cert, private := p.leaf(t, "spiffe://racer/controlplane", "localhost")

	pair, err := tls.X509KeyPair(cert, private)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(root)

	s := httptest.NewUnstartedServer(handler)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	s.StartTLS()
	s.URL = strings.Replace(s.URL, "127.0.0.1", "localhost", 1)

	return s, p
}

// Removal catch-up across missed responses, controller restarts and topology GC.

func (f *coordinationFixture) generation(t *testing.T, selected bool) {
	t.Helper()

	n, p, svc := fixtures()
	p.UID = "pod-uid"

	g, _, err := buildCacheFixture("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
	if !selected {
		n.Labels[annotationPrefix+"exclude"] = "true"
		g, _, err = buildCacheFixture("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p})
	}

	if err != nil {
		t.Fatal(err)
	}

	g.Revision++

	_, pointer, err := f.s.controlStore.load(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.controlStore.commit(context.Background(), g, pointer); err != nil {
		t.Fatal(err)
	}

	f.index, err = indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.install(f.index); err != nil {
		t.Fatal(err)
	}
}

// Legacy enrollment fixture; production control validates a distinct selected
// Pod UID from each recipient's client certificate.
type multiTokenClient struct{ client.Client }

func (c multiTokenClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if review, ok := obj.(*authenticationv1.TokenReview); ok {
		if review.Spec.Token == "pod-survivor" || review.Spec.Token == "pod-failed" {
			review.Status.Authenticated = true
			review.Status.Audiences = review.Spec.Audiences
			review.Status.User.Extra = map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {review.Spec.Token}}
		}

		return nil
	}

	return c.Client.Create(ctx, obj, opts...)
}

// Keep the fake client's authoritative tracker and all normal writes/CAS, but
// avoid its JSON/base64 round-trip when reading multi-megabyte ConfigMaps. The
// race detector makes that fixture-only conversion exceed the real subscriber's
// first-byte deadline on every forward retry. Tracker.Get returns a deep copy,
// so this is neither a payload cache nor a bypass of missing/corrupt-object checks.
func coordinationKube(objects ...client.Object) client.Client {
	scheme := fakeKube().Scheme()
	enabled := true
	objects = append(objects, &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: machina.SiteSpec{Components: machina.SiteComponents{Racer: &machina.RacerComponentSpec{SiteComponentSpec: machina.SiteComponentSpec{Enabled: &enabled}}}}})
	tracker := kubetesting.NewObjectTracker(scheme, serializer.NewCodecFactory(scheme).UniversalDecoder())

	return fake.NewClientBuilder().WithScheme(scheme).WithObjectTracker(tracker).
		WithStatusSubresource(&racerapi.P2PCache{}).WithObjects(objects...).
		WithIndex(&corev1.Node{}, universeIndex, objectUniverses).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				stored, err := tracker.Get(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, key.Namespace, key.Name)
				if err != nil {
					return err
				}

				*cm = *stored.(*corev1.ConfigMap)

				return nil
			}

			return underlying.Get(ctx, key, obj, opts...)
		}}).Build()
}

func TestCoordinationConfigMapReadsPreserveStoreSemantics(t *testing.T) {
	ctx := t.Context()
	kube := coordinationKube()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "payload", Namespace: "state"}, BinaryData: map[string][]byte{"snapshot": bytes.Repeat([]byte{7}, 512*1024)}}
	if err := kube.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}

	var first, second corev1.ConfigMap

	key := client.ObjectKeyFromObject(cm)
	if err := kube.Get(ctx, key, &first); err != nil {
		t.Fatal(err)
	}

	first.BinaryData["snapshot"][0] = 9

	if err := kube.Get(ctx, key, &second); err != nil {
		t.Fatal(err)
	}

	if second.BinaryData["snapshot"][0] != 7 {
		t.Fatal("read aliases authoritative storage")
	}

	if err := kube.Update(ctx, &first); err != nil {
		t.Fatal(err)
	}

	if err := kube.Update(ctx, &second); !apierrors.IsConflict(err) {
		t.Fatalf("stale update must conflict: %v", err)
	}

	if err := kube.Get(ctx, key, &second); err != nil {
		t.Fatal(err)
	}

	if second.BinaryData["snapshot"][0] != 9 {
		t.Fatal("read cached stale payload")
	}

	if err := kube.Delete(ctx, &first); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(ctx, key, &second); !apierrors.IsNotFound(err) {
		t.Fatalf("read concealed deleted payload: %v", err)
	}
}
