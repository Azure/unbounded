// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"crypto/x509"
	"errors"
	"sort"
	"time"
)

// VerifyMember binds an authenticated TLS leaf to the exact enrolled pod/boot.
// The transport must independently verify its TLS chain and usage.
func (m *Manager) VerifyMember(ctx context.Context, key MemberKey, leafDER []byte) error {
	_, s, err := m.read(ctx)
	if err != nil {
		return err
	}

	p := s.Members[key.String()]
	if p == nil {
		return errors.New("unknown durable member")
	}

	leaf, ok := p.Leaves[digest(leafDER)]
	if !ok || !m.options.Now().Before(leaf.Expiry) {
		return errors.New("TLS leaf is not enrolled for this pod and boot")
	}

	cert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return err
	}

	uri, err := p.Identity.URI()
	if err != nil {
		return err
	}

	if len(cert.URIs) != 1 || cert.URIs[0].String() != uri {
		return errors.New("TLS leaf identity mismatch")
	}

	return nil
}

// Members returns all durable admissions, including idle and draining members.
// BootID "pending" is reserved for CP pods whose actual boot is not known yet.
func (m *Manager) Members(ctx context.Context) ([]Identity, error) {
	_, s, err := m.read(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]Identity, 0, len(s.Members))
	for _, p := range s.Members {
		result = append(result, p.Identity)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Key().String() < result[j].Key().String() })

	return result, nil
}

func admit(s *state, identity Identity) (*member, error) {
	if _, err := identity.URI(); err != nil {
		return nil, err
	}

	key := identity.Key().String()
	if s.Retired[key] {
		return nil, errors.New("process has been authoritatively retired")
	}

	if p := s.Members[key]; p != nil {
		previous := p.Identity
		if previous.ContainerID == "" {
			previous.ContainerID = identity.ContainerID
		}

		if previous != identity {
			return nil, errors.New("durable process identity cannot be changed")
		}

		p.Identity = previous

		return p, nil
	}

	p := &member{Identity: identity, Leaves: map[string]leafRecord{}}
	s.Members[key] = p

	return p, nil
}

// Admit adds every active, idle, draining, or takeover-capable process to the
// durable barrier. Reboots are separate members until the old boot is retired.
func (m *Manager) Admit(ctx context.Context, identity Identity) error {
	return m.mutate(ctx, func(s *state) error { _, err := admit(s, identity); return err })
}

// Retire is an authoritative operation, never a heartbeat timeout. The caller
// must establish from Kubernetes/topology that this process cannot participate.
func (m *Manager) Retire(ctx context.Context, key MemberKey) error {
	if !processID.MatchString(key.PodUID) || !processID.MatchString(key.BootID) {
		return errors.New("invalid member key")
	}

	return m.mutate(ctx, func(s *state) error { delete(s.Members, key.String()); s.Retired[key.String()] = true; return nil })
}

func (m *Manager) Issue(ctx context.Context, csrPEM []byte, identity Identity) (IssuedCertificate, error) {
	return m.issue(ctx, csrPEM, identity, false)
}

// IssueProbe issues a next-root certificate during overlap so processes can
// prove installation before production issuance switches. These certificates
// have the same restricted identity and lifetime and are durably accounted for.
// Serve them on a separate proof listener while the main listener uses Issue.
func (m *Manager) IssueProbe(ctx context.Context, csrPEM []byte, identity Identity) (IssuedCertificate, error) {
	return m.issue(ctx, csrPEM, identity, true)
}

func (m *Manager) issue(ctx context.Context, csrPEM []byte, identity Identity, probe bool) (IssuedCertificate, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return IssuedCertificate{}, err
	}

	if _, err = identity.URI(); err != nil {
		return IssuedCertificate{}, err
	}

	var result IssuedCertificate

	err = m.mutate(ctx, func(s *state) error {
		if err := m.ready(ctx, s); err != nil {
			return err
		}

		p, err := admit(s, identity)
		if err != nil {
			return err
		}

		root := s.Active
		if probe {
			root = s.proofRoot()
		}

		for i := range s.Authorities {
			ca := &s.Authorities[i]
			if ca.Digest != root {
				continue
			}

			certificate, expiry, err := signLeaf(*ca, csr, identity, m.namespace, m.options.Now(), m.options)
			if err != nil {
				return err
			}

			if expiry.After(ca.LastIssuedExpiry) {
				ca.LastIssuedExpiry = expiry
			}

			certs, err := parseCertificates(certificate)
			if err != nil {
				return err
			}
			// Prune expired leaf bookkeeping, never the durable member itself.
			for id, leaf := range p.Leaves {
				if m.options.Now().After(leaf.Expiry.Add(m.options.ClockSkew)) {
					delete(p.Leaves, id)
				}
			}

			p.Leaves[digest(certs[0].Raw)] = leafRecord{Root: root, Expiry: expiry}
			result = IssuedCertificate{CertificatePEM: certificate, NotAfter: expiry, RootDigest: root, Bundle: s.bundle()}

			return nil
		}

		return errors.New("active authority missing")
	})
	if err != nil {
		return IssuedCertificate{}, err
	}

	return result, nil
}

// ObserveHeartbeat records claims without granting TLS proof or drain credit.
// RecordTLSProof binds these claims to a fresh cryptographically verified session.
func (m *Manager) ObserveHeartbeat(ctx context.Context, key MemberKey, ack Acknowledgment) error {
	return m.mutate(ctx, func(s *state) error {
		p := s.Members[key.String()]
		if p == nil {
			return errors.New("unknown durable member")
		}

		b := s.bundle()
		if ack.Generation != b.Generation || ack.Digest != b.Digest() {
			return errors.New("heartbeat trust generation/digest mismatch")
		}

		p.Ack = ack

		return nil
	})
}

// RecordTLSProof accepts only opaque proofs created by HotTLS.HandshakeProof.
// A proof is bound to the exact leaf enrolled for this pod/boot, both TLS roots,
// current bundle, and a fresh full handshake. Replayed sessions cannot renew it.
func (m *Manager) RecordTLSProof(ctx context.Context, key MemberKey, proof Proof) error {
	if proof.peerFingerprint == "" {
		return errors.New("missing verified TLS proof")
	}

	return m.mutate(ctx, func(s *state) error {
		p := s.Members[key.String()]
		if p == nil {
			return errors.New("unknown durable member")
		}

		b := s.bundle()
		now := m.options.Now()

		leaf, ok := p.Leaves[proof.peerFingerprint]
		if !ok || !now.Before(leaf.Expiry) {
			return errors.New("TLS leaf is not enrolled for this pod and boot")
		}

		uri, err := p.Identity.URI()
		if err != nil {
			return err
		}

		if proof.peerURI != uri || proof.peerRoot != leaf.Root {
			return errors.New("TLS proof identity or root mismatch")
		}

		provenRoot := proof.peerRoot
		if p.Identity.Kind == Node {
			// Overlap accepts an enrolled old-root client against the pending
			// server root. After switching, both endpoints must use the new root.
			if proof.peerIsServer || proof.localRoot != s.proofRoot() || (s.Phase != "overlap" && proof.peerRoot != s.Active) {
				return errors.New("node TLS proof requires pending server root and current client issuer")
			}

			provenRoot = proof.localRoot
		} else if !proof.peerIsServer || proof.peerRoot != s.proofRoot() {
			return errors.New("control-plane proof requires pending server-authenticated root")
		}

		if proof.bundleDigest != b.Digest() || proof.ack.Generation != b.Generation || proof.ack.Digest != b.Digest() {
			return errors.New("TLS proof bundle mismatch")
		}

		if proof.at.Before(s.FenceAt) || proof.at.After(now.Add(m.options.ClockSkew)) || now.Sub(proof.at) > m.options.ProofLifetime || (!p.ProofAt.IsZero() && !proof.at.After(p.ProofAt)) {
			return errors.New("TLS proof is stale or replayed")
		}

		if p.Identity.Kind == ControlPlane && !proof.peerIsServer {
			return errors.New("control-plane proof requires a server-authenticated session")
		}

		p.Ack = proof.ack
		p.ProofGeneration = b.Generation
		p.ProofDigest = b.Digest()
		p.ProofRoot = provenRoot
		p.ProofAt = proof.at
		p.ProofFence = s.Fence
		p.Drained = proof.ack.OldConnectionsDrained

		return nil
	})
}

// Proof contains no exported fields deliberately: remote JSON/headers cannot
// manufacture evidence. It must stay inside the trusted server process.
type Proof struct {
	peerFingerprint string
	peerURI         string
	peerRoot        string
	localRoot       string
	bundleDigest    string
	at              time.Time
	ack             Acknowledgment
	peerIsServer    bool
}
