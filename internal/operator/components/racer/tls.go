// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/operator/component"
)

const day = 24 * time.Hour

func planTLS(ctx context.Context, env *component.Env, plan *component.Plan) (*corev1.Secret, error) {
	secret := &corev1.Secret{}

	err := env.LiveReader().Get(ctx, objectKey(env, tlsName), secret)
	if apierrors.IsNotFound(err) {
		trust := &corev1.ConfigMap{}
		if err := env.LiveReader().Get(ctx, objectKey(env, trustName), trust); !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("racer serving Secret missing with retained trust (read: %v); restore the Secret", err)
		}

		secret, err = newTLS(env.Namespace, time.Now())
		if err != nil {
			return nil, err
		}

		add(plan, component.OpCreateIfAbsent, secret)

		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	if secret.DeletionTimestamp != nil {
		return nil, fmt.Errorf("racer serving Secret is being deleted")
	}

	pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, fmt.Errorf("invalid Racer serving key pair: %w", err)
	}

	caPair, err := tls.X509KeyPair(secret.Data["ca.crt"], secret.Data["ca.key"])
	if err != nil {
		return nil, fmt.Errorf("invalid Racer serving CA: %w", err)
	}

	ca, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		return nil, err
	}

	key, ok := caPair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !ca.IsCA {
		return nil, fmt.Errorf("invalid Racer serving CA key or constraints")
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}

	if err := leaf.CheckSignatureFrom(ca); err != nil {
		return nil, err
	}

	if err := leaf.VerifyHostname(controllerName + "." + env.Namespace + ".svc"); err != nil {
		return nil, err
	}

	now := time.Now()
	if now.Before(leaf.NotAfter.Add(-30*day)) && now.Before(ca.NotAfter.Add(-365*day)) {
		return secret, nil
	}

	updated := secret.DeepCopy()
	if !now.Before(ca.NotAfter.Add(-365 * day)) {
		// Renew the CA certificate with its existing key and subject. Existing
		// clients retain a valid trust anchor while the new bundle propagates.
		ca, updated.Data["ca.crt"], err = issueCA(key, now)
		if err != nil {
			return nil, err
		}
	}

	updated.Data[corev1.TLSCertKey], updated.Data[corev1.TLSPrivateKeyKey], err = issueLeaf(env.Namespace, ca, key, now)
	if err != nil {
		return nil, err
	}

	plan.Add(component.Operation{
		Kind: component.OpMergePatch, Component: name,
		Base: component.ToUnstructured(secret), Object: component.ToUnstructured(updated),
	})

	return nil, nil
}

func newTLS(namespace string, now time.Time) (*corev1.Secret, error) {
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

	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: tlsName, Namespace: namespace}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"ca.crt": caPEM, "ca.key": caKey, corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: privateKey},
	}, nil
}

func issueCA(key *ecdsa.PrivateKey, now time.Time) (*x509.Certificate, []byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	cert := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "racer-serving-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(10 * 365 * day), IsCA: true, BasicConstraintsValid: true,
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
		SerialNumber: serial, Subject: pkix.Name{CommonName: dns},
		DNSNames: []string{dns, dns + ".cluster.local"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * day),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
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
