// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

const credentialClaim = "racer.unbounded-cloud.io/credentials"

func validCredentialClaim(cfg Config, claim string) bool {
	return strings.HasPrefix(claim, cfg.IssuerSecretName+"/"+cfg.KeyringSecretName+"/")
}

type credentialState struct {
	issuer   *corev1.Secret
	shared   *corev1.Secret
	bundle   wire.KeyringBundle
	rotation RotationState
	material issuerMaterial
	// Parsed once per authoritative read, never used to install candidate trust.
	signing map[string]parsedSigning
}

func readCredentials(ctx context.Context, reader client.Reader, cfg Config, claim string) (credentialState, error) {
	var (
		issuer, shared corev1.Secret
		b              wire.KeyringBundle
		s              RotationState
		material       issuerMaterial
	)

	for _, entry := range []struct {
		name   string
		secret *corev1.Secret
	}{{cfg.IssuerSecretName, &issuer}, {cfg.KeyringSecretName, &shared}} {
		if err := ctx.Err(); err != nil {
			return credentialState{}, err
		}

		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: entry.name}, entry.secret); err != nil {
			return credentialState{}, authorityReadFailure(err)
		}

		if claim == "" || entry.secret.Annotations[credentialClaim] != claim || entry.secret.DeletionTimestamp != nil || entry.secret.ResourceVersion == "" {
			return credentialState{}, wire.Unavailable
		}
	}

	var err error

	b, err = wire.DecodeBundle(bytes.NewReader(shared.Data["bundle.json"]))
	if err != nil {
		return credentialState{}, err
	}

	if b.Cluster != cfg.Cluster || json.Unmarshal(shared.Data["rotation.json"], &s) != nil || json.Unmarshal(issuer.Data["issuer.json"], &material) != nil {
		return credentialState{}, wire.Unavailable
	}

	credentials := credentialState{issuer: &issuer, shared: &shared, bundle: b, rotation: s, material: material}
	if err := credentials.validateRotation(); err != nil {
		return credentialState{}, err
	}

	return credentials, nil
}

func (c *credentialState) validateRotation() error {
	b, s, m := c.bundle, c.rotation, c.material
	if s.NextRotation.IsZero() || s.Retiring == nil || !containsRoot(b, s.ActiveIssuer) || (s.PreparedIssuer == "") != s.ActivateAt.IsZero() {
		return wire.Unavailable
	}

	c.signing = make(map[string]parsedSigning, len(m.Keys))
	for id, material := range m.Keys {
		if rootID(material.Certificate) != id {
			return wire.Unavailable
		}

		cert, key, err := parseSigning(material)
		if err != nil {
			return err
		}

		c.signing[id] = parsedSigning{certificate: cert, key: key}
	}

	required := map[string]struct{}{}

	if !s.ActivateAt.IsZero() {
		if !containsRoot(b, s.PreparedIssuer) || s.PreparedIssuer == s.ActiveIssuer || !s.ActivateAt.After(s.NextRotation) {
			return wire.Unavailable
		}
	}

	for _, root := range b.PeerTrustRoots {
		id := rootID(root)

		key, ok := c.signing[id]
		if !ok || !bytes.Equal(key.certificate.Raw, root) {
			return wire.Unavailable
		}

		if id != s.ActiveIssuer && id != s.PreparedIssuer {
			required[id] = struct{}{}
		}
	}

	prepared := map[string]bool{}

	for _, key := range b.CacheKeys {
		if key.State == wire.RetiringKey {
			required[keyID(key)] = struct{}{}
		}

		if key.State == wire.PreparedKey {
			scope := string(key.Key.Cache) + "/" + string(key.Key.Purpose)
			if s.ActivateAt.IsZero() || prepared[scope] {
				return wire.Unavailable
			}

			prepared[scope] = true
		}
	}

	if len(required) != len(s.Retiring) {
		return wire.Unavailable
	}

	for id := range required {
		if at, ok := s.Retiring[id]; !ok || at.IsZero() {
			return wire.Unavailable
		}
	}

	if m.Pending != "" {
		if _, ok := m.Keys[m.Pending]; !ok {
			return wire.Unavailable
		}
	}

	return nil
}
