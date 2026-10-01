// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	day                = 24 * time.Hour
	caRotationInterval = 7 * day
	leafLifetime       = 14 * day
	leafRenewBefore    = 7 * day
	caLifetime         = 28 * day
	trustOverlap       = 14 * day
	tlsStateKey        = "rotation.json"
	previousCAKey      = "previous-ca.crt"
	caBundleKey        = "ca-bundle.crt"
	tlsStateAnnotation = "racer.unbounded-cloud.io/serving-tls-state"
	maxPreviousCAs     = 2
)

// Only the current CA's private key is retained. Each previous generation holds
// the next generation's public certificate signed by that previous CA. In order,
// these form a bounded path from the latest leaf back to every retained root.
type previousCA struct {
	Root     []byte    `json:"root"`
	Cross    []byte    `json:"cross"`
	RetireAt time.Time `json:"retireAt"`
}

type tlsState struct {
	Version   int          `json:"version"`
	CreatedAt time.Time    `json:"createdAt"`
	Previous  []previousCA `json:"previous"`
}

// Retained controllers can still serve even without ClusterCaches. Keep their
// existing credentials usable without resuming installation or workload repair.
func planRetainedTLS(ctx context.Context, env *component.Env, now time.Time) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()
	result := component.Disabled("no ClusterCaches; Racer resources are retained")

	claim := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, claimName), claim); err != nil {
		if apierrors.IsNotFound(err) {
			return plan, result, nil
		}

		return nil, result, err
	}

	if claim.Data["state"] == "reserved" {
		return plan, result, nil
	}

	marker, err := claimedMarker(ctx, env, claim)
	if err != nil {
		return nil, result, err
	}

	if marker.Data["state"] == "fresh" && !ptr.Deref(marker.Immutable, false) {
		return plan, result, nil
	}

	if marker.Data["state"] != "consumed" || !ptr.Deref(marker.Immutable, false) {
		return nil, result, fmt.Errorf("invalid retained Racer installation state")
	}

	if err := env.LiveReader().Get(ctx, objectKey(env, tlsName), &corev1.Secret{}); err != nil {
		if apierrors.IsNotFound(err) {
			return plan, result, nil
		}

		return nil, result, err
	}

	result.RequeueAfter = time.Hour
	result.Message = "no ClusterCaches; maintaining retained Racer serving TLS only"

	secret, err := planTLSAt(ctx, env, plan, false, now)
	if err != nil {
		return nil, result, err
	}

	if secret == nil {
		result.RequeueAfter = 5 * time.Second
		return plan, result, nil
	}
	// Trust is derived only from committed credentials. Avoid an apply every
	// hour, and preserve unrelated retained ConfigMap fields under CAS.
	trust := &corev1.ConfigMap{}
	err = env.LiveReader().Get(ctx, objectKey(env, trustName), trust)

	want := string(secret.Data["ca.crt"]) + string(secret.Data[previousCAKey])
	if apierrors.IsNotFound(err) {
		add(plan, component.OpCreateIfAbsent, &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace},
			Data:       map[string]string{"ca.crt": want},
		})

		result.RequeueAfter = 5 * time.Second

		return plan, result, nil
	}

	if err != nil {
		return nil, result, err
	}

	if trust.DeletionTimestamp != nil {
		return nil, result, fmt.Errorf("retained Racer bootstrap trust is being deleted")
	}

	if trust.Data["ca.crt"] != want {
		trust.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}

		updated := trust.DeepCopy()
		if updated.Data == nil {
			updated.Data = map[string]string{}
		}

		updated.Data["ca.crt"] = want
		plan.Add(component.Operation{Kind: component.OpMergePatch, Component: name, Base: component.ToUnstructured(trust), Object: component.ToUnstructured(updated)})

		result.RequeueAfter = 5 * time.Second
	}

	return plan, result, nil
}

// Persist credentials before deriving trust from them. A
// lost create/CAS race therefore never deploys an uncommitted certificate.
func planTLS(ctx context.Context, env *component.Env, plan *component.Plan, fresh bool) (*corev1.Secret, error) {
	return planTLSAt(ctx, env, plan, fresh, time.Now())
}

func planTLSAt(ctx context.Context, env *component.Env, plan *component.Plan, fresh bool, now time.Time) (*corev1.Secret, error) {
	secret := &corev1.Secret{}

	err := env.LiveReader().Get(ctx, objectKey(env, tlsName), secret)
	if apierrors.IsNotFound(err) {
		if !fresh {
			return nil, fmt.Errorf("established Racer serving Secret missing; restore it")
		}

		if err := env.LiveReader().Get(ctx, objectKey(env, trustName), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("racer serving Secret missing with retained trust (read: %v); restore it", err)
		}

		secret, err = newTLS(env.Namespace, now)
		if err != nil {
			return nil, err
		}

		add(plan, component.OpCreateIfAbsent, secret)

		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if secret.DeletionTimestamp != nil {
		return nil, fmt.Errorf("racer serving Secret is being deleted")
	}

	updated, err := renewTLS(secret, env.Namespace, now)
	if err != nil {
		return nil, err
	}

	if updated == nil {
		return secret, nil
	}

	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	updated.TypeMeta = secret.TypeMeta
	plan.Add(component.Operation{Kind: component.OpMergePatch, Component: name, Base: component.ToUnstructured(secret), Object: component.ToUnstructured(updated)})

	return nil, nil
}

func renewTLS(secret *corev1.Secret, namespace string, now time.Time) (*corev1.Secret, error) {
	now = now.UTC().Truncate(time.Second)

	pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, fmt.Errorf("invalid Racer serving key pair: %w", err)
	}

	caPair, err := tls.X509KeyPair(secret.Data["ca.crt"], secret.Data["ca.key"])
	if err != nil {
		return nil, fmt.Errorf("invalid Racer serving CA: %w", err)
	}

	ca, err := singleCertificate(secret.Data["ca.crt"])
	if err != nil {
		return nil, err
	}

	key, ok := caPair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !ca.IsCA || !ca.BasicConstraintsValid || ca.CheckSignatureFrom(ca) != nil {
		return nil, fmt.Errorf("invalid Racer serving CA key or constraints")
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}

	if err := leaf.CheckSignatureFrom(ca); err != nil {
		return nil, err
	}

	if err := leaf.VerifyHostname(controllerName + "." + namespace + ".svc"); err != nil {
		return nil, err
	}

	if now.Before(ca.NotBefore) || now.Before(leaf.NotBefore) {
		return nil, fmt.Errorf("racer serving certificates are not yet valid")
	}

	if !now.Before(ca.NotAfter) {
		return nil, fmt.Errorf("racer serving CA expired; restore valid serving state")
	}
	// An expired leaf can be renewed, but malformed usage must not be repaired
	// silently. Verify its original usable interval as well as its signature.
	verifyAt := now
	if !verifyAt.Before(leaf.NotAfter) {
		verifyAt = leaf.NotAfter.Add(-time.Second)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca)

	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: verifyAt, DNSName: controllerName + "." + namespace + ".svc"})
	if err != nil {
		return nil, err
	}

	state, err := readTLSState(secret, ca, now)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(secret.Data[corev1.TLSCertKey], servingChain(pair.Certificate[0], state)) {
		return nil, fmt.Errorf("racer serving chain does not match persisted rotation state")
	}

	updated := secret.DeepCopy()
	// Repair derived trust only after validating the authoritative credentials
	// and rotation state.
	changed := !bytes.Equal(secret.Data[caBundleKey], servingRoots(secret))
	// Older generations are a suffix. If an intermediate has expired, no
	// earlier root can verify through it, so retire that entire suffix.
	for i, previous := range state.Previous {
		if !now.Before(previous.RetireAt) {
			state.Previous = state.Previous[:i]
			changed = true

			break
		}
	}

	rotate := !now.Before(state.CreatedAt.Add(caRotationInterval))
	if rotate {
		newKey, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			return nil, keyErr
		}

		newCA, caPEM, issueErr := issueCA(newKey, now)
		if issueErr != nil {
			return nil, issueErr
		}

		cross, crossErr := crossSign(newCA, ca, key)
		if crossErr != nil {
			return nil, crossErr
		}

		retireAt := now.Add(trustOverlap)
		if ca.NotAfter.Before(retireAt) {
			retireAt = ca.NotAfter
		}

		state.Previous = append([]previousCA{{Root: updated.Data["ca.crt"], Cross: cross, RetireAt: retireAt}}, state.Previous...)
		state.CreatedAt = newCA.NotBefore.Add(time.Hour)
		updated.Data["ca.crt"] = caPEM

		updated.Data["ca.key"], err = keyPEM(newKey)
		if err != nil {
			return nil, err
		}

		ca, key = newCA, newKey
	}

	if rotate || !now.Before(leaf.NotAfter.Add(-leafRenewBefore)) {
		updated.Data[corev1.TLSCertKey], updated.Data[corev1.TLSPrivateKeyKey], err = issueLeaf(namespace, ca, key, now)
		if err != nil {
			return nil, err
		}

		changed = true
	}

	if !changed {
		return nil, nil
	}

	if err := writeTLSState(updated, state); err != nil {
		return nil, err
	}

	return updated, nil
}

func newTLS(namespace string, now time.Time) (*corev1.Secret, error) {
	now = now.UTC().Truncate(time.Second)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	ca, caPEM, err := issueCA(key, now)
	if err != nil {
		return nil, err
	}

	caKey, err := keyPEM(key)
	if err != nil {
		return nil, err
	}

	cert, privateKey, err := issueLeaf(namespace, ca, key, now)
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: tlsName, Namespace: namespace}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"ca.crt": caPEM, "ca.key": caKey, corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: privateKey},
	}
	err = writeTLSState(secret, tlsState{Version: 1, CreatedAt: ca.NotBefore.Add(time.Hour)})

	return secret, err
}

func issueCA(key *ecdsa.PrivateKey, now time.Time) (*x509.Certificate, []byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	cert := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "racer-serving-ca-" + serial.Text(16)},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(caLifetime), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	parsed, err := x509.ParseCertificate(der)

	return parsed, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), err
}

func issueLeaf(namespace string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	dns := controllerName + "." + namespace + ".svc"

	cert := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: dns}, DNSNames: []string{dns, dns + ".cluster.local"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(leafLifetime), BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ca.NotAfter.Before(cert.NotAfter) {
		cert.NotAfter = ca.NotAfter
	}

	der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}

	privateKey, err := keyPEM(key)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), privateKey, err
}

func keyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func singleCertificate(data []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("expected exactly one PEM certificate")
	}

	return x509.ParseCertificate(block.Bytes)
}

func crossSign(child, parent *x509.Certificate, key *ecdsa.PrivateKey) ([]byte, error) {
	template := *child
	if parent.NotAfter.Before(template.NotAfter) {
		template.NotAfter = parent.NotAfter
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, parent, child.PublicKey, key)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func servingChain(leafDER []byte, state tlsState) []byte {
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	for _, previous := range state.Previous {
		chain = append(chain, previous.Cross...)
	}

	return chain
}

func previousRoots(state tlsState) []byte {
	// Keep an explicit empty value: JSON merge patch treats null as deletion.
	roots := []byte{}
	for _, previous := range state.Previous {
		roots = append(roots, previous.Root...)
	}

	return roots
}

func servingRoots(secret *corev1.Secret) []byte {
	return append(bytes.Clone(secret.Data["ca.crt"]), secret.Data[previousCAKey]...)
}

func writeTLSState(secret *corev1.Secret, state tlsState) error {
	if len(state.Previous) > maxPreviousCAs {
		return fmt.Errorf("too many Racer serving CA generations")
	}

	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}

	block, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
	if block == nil {
		return fmt.Errorf("missing Racer serving leaf")
	}

	secret.Data[corev1.TLSCertKey] = servingChain(block.Bytes, state)
	secret.Data[tlsStateKey] = encoded

	secret.Data[previousCAKey] = previousRoots(state)

	secret.Data[caBundleKey] = servingRoots(secret)
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}

	secret.Annotations[tlsStateAnnotation] = "1"

	return nil
}

func readTLSState(secret *corev1.Secret, ca *x509.Certificate, now time.Time) (tlsState, error) {
	state := tlsState{Version: 1}

	data, exists := secret.Data[tlsStateKey]
	if !exists {
		return state, fmt.Errorf("missing Racer serving rotation state")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("invalid Racer serving rotation state: %w", err)
	}

	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return state, fmt.Errorf("trailing Racer serving rotation state")
	}

	if state.Version != 1 || secret.Annotations[tlsStateAnnotation] != "1" ||
		!state.CreatedAt.Equal(ca.NotBefore.Add(time.Hour)) || state.CreatedAt.After(now) ||
		!ca.NotAfter.Equal(state.CreatedAt.Add(caLifetime)) || len(state.Previous) > maxPreviousCAs {
		return state, fmt.Errorf("invalid Racer serving rotation policy state")
	}

	if _, exists := secret.Data[previousCAKey]; !exists || !bytes.Equal(secret.Data[previousCAKey], previousRoots(state)) {
		return state, fmt.Errorf("racer previous CA roots do not match rotation state")
	}

	child := ca

	for i, previous := range state.Previous {
		root, err := singleCertificate(previous.Root)
		if err != nil {
			return state, err
		}

		cross, err := singleCertificate(previous.Cross)
		if err != nil {
			return state, err
		}

		expires := child.NotAfter
		if root.NotAfter.Before(expires) {
			expires = root.NotAfter
		}

		retireAt := child.NotBefore.Add(time.Hour + trustOverlap)
		if expires.Before(retireAt) {
			retireAt = expires
		}

		if root.CheckSignatureFrom(root) != nil || cross.CheckSignatureFrom(root) != nil ||
			!cross.IsCA || !cross.BasicConstraintsValid || cross.KeyUsage != child.KeyUsage || !bytes.Equal(cross.RawSubjectPublicKeyInfo, child.RawSubjectPublicKeyInfo) ||
			!bytes.Equal(cross.RawSubject, child.RawSubject) || root.NotBefore.After(now) ||
			!cross.NotBefore.Equal(child.NotBefore) || !cross.NotAfter.Equal(expires) || !previous.RetireAt.Equal(retireAt) ||
			!previous.RetireAt.After(child.NotBefore.Add(time.Hour)) ||
			(i > 0 && !previous.RetireAt.Before(state.Previous[i-1].RetireAt)) {
			return state, fmt.Errorf("invalid Racer previous CA compatibility chain")
		}

		child = root
	}

	return state, nil
}
