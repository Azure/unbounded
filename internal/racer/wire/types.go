// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package wire declares Racer's HTTPS/JSON contracts.
// Codecs validate bounded inputs before returning usable protocol state.
package wire

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	SchemaVersion       = 1
	BootstrapPath       = "/v1/bootstrap"
	SnapshotPath        = "/v1/snapshot"
	KeyringPath         = "/v1/keyring"
	TokenAudience       = "racer-control"
	MaxBootstrapBytes   = 64 * 1024
	MaxBundleBytes      = 512 * 1024
	MaxPublicationBytes = 64 * 1024 * 1024
	MaxMembers          = 100_000
	PollWait            = 30 * time.Second
	CertificateLifetime = 24 * time.Hour
	DefaultShares       = 4
	SharesAnnotation    = "racer.unbounded-cloud.io/shares"
	RailsAnnotation     = "racer.unbounded-cloud.io/rails"
	AlignmentAnnotation = "racer.unbounded-cloud.io/aligned-rails"
	ExclusionLabel      = "racer.unbounded-cloud.io/exclude"
)

type (
	ClusterID         string
	NodeID            string
	CacheID           string
	EnrollmentID      string
	Sequence          uint64
	MembershipVersion uint64
	Generation        uint64
)

type Rail struct {
	Rail     uint16  `json:"rail"`
	Fabric   string  `json:"fabric"`
	NUMANode *uint32 `json:"numa_node,omitempty"`
}

type Member struct {
	Node             NodeID `json:"node"`
	Shares           uint32 `json:"shares"`
	PeerEndpoint     string `json:"peer_endpoint"`
	Rails            []Rail `json:"rails"`
	AlignmentEnabled bool   `json:"alignment_enabled"`
	// Site is the RDMA boundary. Empty means HTTP-only, not a shared default site.
	Site string `json:"site"`
}

type CacheDefinition struct {
	ID           CacheID `json:"id"`
	Name         string  `json:"name"`
	ClientSocket string  `json:"client_socket"`
	OriginSocket string  `json:"origin_socket"`
}

type Publication struct {
	SchemaVersion     uint32            `json:"schema_version"`
	Cluster           ClusterID         `json:"cluster"`
	Sequence          Sequence          `json:"sequence,string"`
	MembershipVersion MembershipVersion `json:"membership_version,string"`
	Members           []Member          `json:"members"`
	Caches            []CacheDefinition `json:"caches"`
}

// BootstrapRequest contains no bearer token. The transport reads the projected
// token for each issuance attempt and supplies it in Authorization.
type BootstrapRequest struct {
	Shares        uint32       `json:"shares"`
	SchemaVersion uint32       `json:"schema_version"`
	Cluster       ClusterID    `json:"cluster"`
	Enrollment    EnrollmentID `json:"enrollment"`
	CSRDER        []byte       `json:"csr_der"`
}

type BootstrapResponse struct {
	SchemaVersion    uint32       `json:"schema_version"`
	Cluster          ClusterID    `json:"cluster"`
	Node             NodeID       `json:"node"`
	Enrollment       EnrollmentID `json:"enrollment"`
	CertificateChain [][]byte     `json:"certificate_chain"`
}

type (
	KeyPurpose string
	KeyState   string
)

const (
	PageKey              KeyPurpose = "page"
	OriginCredentialsKey KeyPurpose = "origin_credentials"
	PreparedKey          KeyState   = "prepared"
	ActiveKey            KeyState   = "active"
)

type CacheKeyRef struct {
	Cache   CacheID    `json:"cache"`
	ID      []byte     `json:"id"` // Exactly 16 bytes, padded standard base64 on the wire.
	Purpose KeyPurpose `json:"purpose"`
}

// CacheKey intentionally hides material from ordinary formatting and JSON.
// Only the bounded bundle codec may access and encode the 32-byte material.
type CacheKey struct {
	Key      CacheKeyRef
	State    KeyState
	material [32]byte
}

func (CacheKey) String() string   { return "<redacted cache key>" }
func (CacheKey) GoString() string { return "<redacted cache key>" }

// NewCacheKey is the only material ingress besides bounded bundle decoding.
func NewCacheKey(ref CacheKeyRef, state KeyState, material [32]byte) (CacheKey, error) {
	if !ValidUUID(string(ref.Cache)) || len(ref.ID) != 16 || string(ref.ID[:4]) != "RKG1" || binary.BigEndian.Uint64(ref.ID[4:12]) == 0 || (ref.Purpose != PageKey && ref.Purpose != OriginCredentialsKey) || (state != PreparedKey && state != ActiveKey) {
		return CacheKey{}, InvalidRequest
	}

	ref.ID = bytes.Clone(ref.ID)

	return CacheKey{Key: ref, State: state, material: material}, nil
}

// EqualMaterial compares private key bytes without exposing them to consumers.
// It is intended for bundle replay validation, not authentication.
func (k CacheKey) EqualMaterial(other CacheKey) bool {
	return bytes.Equal(k.material[:], other.material[:])
}

type KeyringBundle struct {
	SchemaVersion  uint32
	Cluster        ClusterID
	Generation     Generation
	PeerTrustRoots [][]byte
	CacheKeys      []CacheKey
}

// MarshalJSON prevents bypassing validation through the standard JSON encoder.
func (b KeyringBundle) MarshalJSON() ([]byte, error) { return EncodeBundle(b) }

type ErrorCode string

const (
	InvalidRequest     ErrorCode = "invalid_request"
	Unauthenticated    ErrorCode = "unauthenticated"
	Forbidden          ErrorCode = "forbidden"
	Conflict           ErrorCode = "conflict"
	TooLarge           ErrorCode = "too_large"
	UnsupportedVersion ErrorCode = "unsupported_version"
	Overloaded         ErrorCode = "overloaded"
	Unavailable        ErrorCode = "unavailable"
)

type ErrorResponse struct {
	Code ErrorCode `json:"code"`
}

// Error returns only a protocol code, never input or secret material.
func (c ErrorCode) Error() string { return string(c) }

func validError(c ErrorCode) bool {
	switch c {
	case InvalidRequest, Unauthenticated, Forbidden, Conflict, TooLarge, UnsupportedVersion, Overloaded, Unavailable:
		return true
	default:
		return false
	}
}

// ValidUUID checks canonical Kubernetes identities before constructing wire state.
func ValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}

	for i, c := range []byte(s) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}

			continue
		}

		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

func validRail(r Rail) bool {
	return r.Fabric != "" && utf8.ValidString(r.Fabric) && !strings.ContainsAny(r.Fabric, "\x00\r\n")
}

func validateHeader(version uint32, cluster ClusterID) error {
	if version != SchemaVersion {
		return UnsupportedVersion
	}

	if !ValidUUID(string(cluster)) {
		return InvalidRequest
	}

	return nil
}

// ValidateBootstrapRequest checks the wire fields, CSR syntax, and full encoded
// size without serializing. Proof of possession and identity binding belong to
// the issuer. Direct callers have the same size bound as EncodeBootstrapRequest.
func ValidateBootstrapRequest(v BootstrapRequest) error {
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return err
	}

	if !ValidUUID(string(v.Enrollment)) || v.Shares == 0 {
		return InvalidRequest
	}

	if len(v.CSRDER) > MaxBootstrapBytes {
		return TooLarge
	}

	if _, err := x509.ParseCertificateRequest(v.CSRDER); err != nil {
		return InvalidRequest
	}

	// The validated version is 1 and UUIDs are unescaped ASCII. Only padded
	// base64 contributes variable framing size; no encoder newline is on the wire.
	const framing = len(`{"shares":,"schema_version":1,"cluster":"","enrollment":"","csr_der":""}`)
	if framing+len(strconv.FormatUint(uint64(v.Shares), 10))+len(v.Cluster)+len(v.Enrollment)+base64.StdEncoding.EncodedLen(len(v.CSRDER)) > MaxBootstrapBytes {
		return TooLarge
	}

	return nil
}

func validateBootstrapResponse(v BootstrapResponse) error {
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return err
	}

	if !ValidUUID(string(v.Node)) || !ValidUUID(string(v.Enrollment)) {
		return InvalidRequest
	}

	return validateCertificates(v.CertificateChain, MaxBootstrapBytes)
}

func validateCertificates(certs [][]byte, limit int) error {
	if len(certs) == 0 {
		return InvalidRequest
	}

	total := 0
	for _, cert := range certs {
		if len(cert) > limit-total {
			return TooLarge
		}

		total += len(cert)
		if _, err := x509.ParseCertificate(cert); err != nil {
			return InvalidRequest
		}
	}

	return nil
}

// CanonicalSocketPaths returns Linux pathname sockets including room for NUL in
// sockaddr_un.sun_path (108 bytes). Names are ASCII Kubernetes DNS subdomains.
func CanonicalSocketPaths(name string) (client, origin string, err error) {
	if name == "" || len(name) > 253 {
		return "", "", InvalidRequest
	}

	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return "", "", InvalidRequest
		}

		for i, c := range []byte(label) {
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
				continue
			}

			if c != '-' || i == 0 || i == len(label)-1 {
				return "", "", InvalidRequest
			}
		}
	}

	client = "/run/racer/" + name + "/client/socket"

	origin = "/run/racer/" + name + "/origin/socket"
	if len(client) > 107 || len(origin) > 107 {
		return "", "", InvalidRequest
	}

	return client, origin, nil
}

func validatePublication(v Publication, counters bool) error {
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return err
	}

	if counters && (v.Sequence == 0 || v.MembershipVersion == 0) {
		return InvalidRequest
	}

	if len(v.Members) > MaxMembers {
		return TooLarge
	}
	// A cheap lower bound prevents copying/encoding caller-owned oversized state.
	remaining := MaxPublicationBytes

	consume := func(n int) bool {
		if n > remaining {
			return false
		}

		remaining -= n

		return true
	}
	for _, m := range v.Members {
		if !consume(len(m.Node)) || !consume(len(m.PeerEndpoint)) || !consume(len(m.Site)) {
			return TooLarge
		}

		for _, r := range m.Rails {
			if !consume(len(r.Fabric) + 1) {
				return TooLarge
			}
		}
	}

	for _, c := range v.Caches {
		if !consume(len(c.ID)) || !consume(len(c.Name)) || !consume(len(c.ClientSocket)) || !consume(len(c.OriginSocket)) {
			return TooLarge
		}
	}

	nodes := map[NodeID]bool{}
	for _, m := range v.Members {
		if !ValidUUID(string(m.Node)) || nodes[m.Node] || m.Shares == 0 || len(validation.IsValidLabelValue(m.Site)) != 0 {
			return InvalidRequest
		}

		nodes[m.Node] = true

		ap, err := netip.ParseAddrPort(m.PeerEndpoint)
		if err != nil || ap.Port() == 0 || ap.Addr().Zone() != "" {
			return InvalidRequest
		}

		rails := map[uint16]bool{}
		for _, r := range m.Rails {
			if rails[r.Rail] || !validRail(r) {
				return InvalidRequest
			}

			rails[r.Rail] = true
		}
	}

	ids := map[CacheID]bool{}

	names := map[string]bool{}
	for _, c := range v.Caches {
		if !ValidUUID(string(c.ID)) || ids[c.ID] || names[c.Name] {
			return InvalidRequest
		}

		ids[c.ID], names[c.Name] = true, true

		client, origin, err := CanonicalSocketPaths(c.Name)
		if err != nil || c.ClientSocket != client || c.OriginSocket != origin {
			return InvalidRequest
		}
	}

	return nil
}

func validateBundle(v KeyringBundle) error {
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return err
	}

	if v.Generation == 0 {
		return InvalidRequest
	}

	if len(v.CacheKeys) > MaxBundleBytes/32 {
		return TooLarge
	}

	if err := validateCertificates(v.PeerTrustRoots, MaxBundleBytes); err != nil {
		return err
	}

	roots := map[string]bool{}
	for _, r := range v.PeerTrustRoots {
		if roots[string(r)] {
			return InvalidRequest
		}

		roots[string(r)] = true
	}

	type scope struct {
		cache   CacheID
		purpose KeyPurpose
	}

	type identity struct {
		scope
		id string
	}

	seen := map[identity]bool{}
	materials := map[[32]byte]bool{}
	active := map[scope]int{}

	for _, k := range v.CacheKeys {
		if _, err := NewCacheKey(k.Key, k.State, k.material); err != nil {
			return err
		}

		if binary.BigEndian.Uint64(k.Key.ID[4:12]) > uint64(v.Generation) {
			return InvalidRequest
		}

		s := scope{k.Key.Cache, k.Key.Purpose}

		id := identity{s, string(k.Key.ID)}
		if seen[id] || materials[k.material] {
			return InvalidRequest
		}

		seen[id] = true
		materials[k.material] = true

		if _, ok := active[s]; !ok {
			active[s] = 0
		}

		if k.State == ActiveKey {
			active[s]++
		}
	}

	for _, count := range active {
		if count != 1 {
			return InvalidRequest
		}
	}

	return nil
}
