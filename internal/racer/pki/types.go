// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

const (
	SecretName    = "racer-ca"
	ConfigMapName = "racer-trust"
	StateKey      = "state.json"
	BundleKey     = "bundle.json"
	Node          = "node"
	ControlPlane  = "controlplane"
)

var (
	ErrNotLeader = errors.New("racer PKI: not the fenced leader")
	ErrLostState = errors.New("racer PKI: existing trust or state prevents CA regeneration")
	ErrNotReady  = errors.New("racer PKI: trust publication is not current")
	hexID        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	processID    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// Identity must be derived by the enrollment server from authenticated admission
// data. CSR subjects, SANs, and extensions are never used as authorization input.
type Identity struct {
	Kind     string `json:"kind"`
	Universe string `json:"universe,omitempty"`
	Node     string `json:"node,omitempty"`
	PodUID   string `json:"podUID"`
	BootID   string `json:"bootID"`
	// ContainerID is authoritative runtime metadata, not part of the TLS URI.
	// Once known, it cannot be replaced or cleared for this pod and boot.
	ContainerID string `json:"containerID,omitempty"`
	// PodName is server-derived Kubernetes metadata, not part of the TLS URI.
	PodName string `json:"podName,omitempty"`
}

type MemberKey struct {
	PodUID string `json:"podUID"`
	BootID string `json:"bootID"`
}

func (i Identity) Key() MemberKey  { return MemberKey{PodUID: i.PodUID, BootID: i.BootID} }
func (k MemberKey) String() string { return k.PodUID + "/" + k.BootID }

func (i Identity) URI() (string, error) {
	if !processID.MatchString(i.PodUID) || !processID.MatchString(i.BootID) {
		return "", errors.New("pod UID and boot ID must be nonempty safe process identifiers")
	}

	switch i.Kind {
	case Node:
		if !hexID.MatchString(i.Universe) || !hexID.MatchString(i.Node) {
			return "", errors.New("universe and node must be 64 lowercase hexadecimal characters")
		}

		return "spiffe://racer/universe/" + i.Universe + "/node/" + i.Node + "/pod/" + i.PodUID, nil
	case ControlPlane:
		if i.Universe != "" || i.Node != "" {
			return "", errors.New("control-plane identity must not contain a universe or node")
		}

		return "spiffe://racer/controlplane", nil
	default:
		return "", fmt.Errorf("unknown identity kind %q", i.Kind)
	}
}

func identityURL(i Identity) (*url.URL, error) {
	s, err := i.URI()
	if err != nil {
		return nil, err
	}

	return url.Parse(s)
}

// TrustBundle is the exact public bundle.json wire schema. Active is the SHA256
// digest of the active root's DER, not of its PEM representation.
type TrustBundle struct {
	Version      int    `json:"version"`
	Generation   uint64 `json:"generation"`
	Active       string `json:"active"`
	Certificates string `json:"certificates"`
}

func (b TrustBundle) JSON() []byte {
	data, err := json.Marshal(b)
	if err != nil {
		panic(err)
	} // This fixed schema contains only JSON primitives.

	return data
}
func (b TrustBundle) Digest() string { return digest(b.JSON()) }
func digest(data []byte) string      { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

type Options struct {
	LeafLifetime      time.Duration
	CALifetime        time.Duration
	RotateAfter       time.Duration
	ClockSkew         time.Duration
	ReconcileInterval time.Duration
	ProofLifetime     time.Duration
	// Now is intended for deterministic tests. Production defaults to time.Now.
	Now func() time.Time
}

func (o Options) defaults() Options {
	if o.LeafLifetime == 0 {
		o.LeafLifetime = 24 * time.Hour
	}

	if o.CALifetime == 0 {
		o.CALifetime = 365 * 24 * time.Hour
	}

	if o.RotateAfter == 0 {
		o.RotateAfter = 180 * 24 * time.Hour
	}

	if o.ClockSkew == 0 {
		o.ClockSkew = 5 * time.Minute
	}

	if o.ReconcileInterval == 0 {
		o.ReconcileInterval = 5 * time.Second
	}

	if o.ProofLifetime == 0 {
		o.ProofLifetime = 5 * time.Minute
	}

	if o.Now == nil {
		o.Now = time.Now
	}

	return o
}

type IssuedCertificate struct {
	CertificatePEM []byte
	NotAfter       time.Time
	RootDigest     string
	Bundle         TrustBundle
}

type Acknowledgment struct {
	Generation            uint64 `json:"generation"`
	Digest                string `json:"digest"`
	OldConnectionsDrained bool   `json:"oldConnectionsDrained"`
}

type authority struct {
	Certificate      string    `json:"certificate"`
	PrivateKey       string    `json:"privateKey"`
	Digest           string    `json:"digest"`
	LastIssuedExpiry time.Time `json:"lastIssuedExpiry"`
}

type leafRecord struct {
	Root   string    `json:"root"`
	Expiry time.Time `json:"expiry"`
}

type member struct {
	Identity        Identity              `json:"identity"`
	Leaves          map[string]leafRecord `json:"leaves"`
	Ack             Acknowledgment        `json:"ack"`
	ProofGeneration uint64                `json:"proofGeneration"`
	ProofDigest     string                `json:"proofDigest"`
	ProofRoot       string                `json:"proofRoot"`
	ProofAt         time.Time             `json:"proofAt"`
	ProofFence      string                `json:"proofFence"`
	Drained         bool                  `json:"drained"`
}

type state struct {
	Version     int                `json:"version"`
	Fence       string             `json:"fence"`
	FenceAt     time.Time          `json:"fenceAt"`
	Generation  uint64             `json:"generation"`
	Active      string             `json:"active"`
	Phase       string             `json:"phase"`
	Authorities []authority        `json:"authorities"`
	Members     map[string]*member `json:"members"`
	// Retired prevents an old process from re-admitting itself after retirement.
	Retired       map[string]bool `json:"retired"`
	NextRotation  time.Time       `json:"nextRotation"`
	RotationNonce string          `json:"rotationNonce,omitempty"`
	// Shards names immutable, content-verified participant objects. The Secret's
	// resource version is the single commit point for metadata and participants.
	Shards map[string]shardReference `json:"shards,omitempty"`
	// Once tombstones are collected, every admission must recheck the Pod UID
	// inside the fenced transaction. Persist this requirement across takeover.
	RequireLivePod bool `json:"requireLivePod,omitempty"`
}

func (s *state) bundle() TrustBundle {
	b := TrustBundle{Version: 1, Generation: s.Generation, Active: s.Active}
	for _, ca := range s.Authorities {
		b.Certificates += ca.Certificate
	}

	return b
}

func (s *state) proofRoot() string {
	if s.Phase == "overlap" {
		return s.Authorities[1].Digest
	}

	return s.Active
}
