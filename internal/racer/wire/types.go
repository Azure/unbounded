// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package wire declares Racer's HTTPS/JSON and projected-keyring contracts.
// Validation and codecs remain fail-closed scaffold operations.
package wire

import "time"

const (
	SchemaVersion       = 1
	BootstrapPath       = "/v1/bootstrap"
	SnapshotPath        = "/v1/snapshot"
	TokenAudience       = "racer-control"
	MaxBootstrapBytes   = 64 * 1024
	MaxBundleBytes      = 512 * 1024
	MaxPublicationBytes = 64 * 1024 * 1024
	MaxMembers          = 100_000
	PollWait            = 30 * time.Second
	RetryMin            = time.Second
	RetryMax            = 30 * time.Second
	CertificateLifetime = 24 * time.Hour
	RenewAfter          = 16 * time.Hour
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
}

type CacheDefinition struct {
	ID           CacheID `json:"id"`
	Name         string  `json:"name"`
	ClientSocket string  `json:"client_socket"`
	OriginSocket string  `json:"origin_socket"`
	SocketMode   uint32  `json:"socket_mode"`
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
	RetiringKey          KeyState   = "retiring"
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
	material [32]byte //nolint:unused // Reserved for the bounded secret codec; never expose material to satisfy scaffold lint.
}

func (CacheKey) String() string   { return "<redacted cache key>" }
func (CacheKey) GoString() string { return "<redacted cache key>" }

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
