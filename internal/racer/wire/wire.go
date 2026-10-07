// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package wire declares Racer's HTTPS/JSON contracts.
// Codecs validate bounded inputs before returning usable protocol state.
package wire

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	SchemaVersion          = 1
	BootstrapPath          = "/v1/bootstrap"
	SnapshotPath           = "/v1/snapshot"
	KeyringPath            = "/v1/keyring"
	TokenAudience          = "racer-control"
	MaxBootstrapBytes      = 64 * 1024
	MaxBundleBytes         = 512 * 1024
	MaxPublicationBytes    = 64 * 1024 * 1024
	MaxMembers             = 100_000
	PollWait               = 30 * time.Second
	CertificateLifetime    = 24 * time.Hour
	DefaultShares          = 4
	SharesAnnotation       = "racer.unbounded-cloud.io/shares"
	BlockDevicesAnnotation = "racer.unbounded-cloud.io/block-devices"
	RDMANICsAnnotation     = "racer.unbounded-cloud.io/rdma-nics"
	MaxRDMANICs            = 64
	ExclusionLabel         = "racer.unbounded-cloud.io/exclude"
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

type RDMANIC struct {
	Device   string  `json:"device"`
	Port     uint8   `json:"port"`
	Rail     uint16  `json:"rail"`
	GID      string  `json:"gid,omitempty"`
	NUMANode *uint32 `json:"numa_node,omitempty"`
}

type Member struct {
	Node         NodeID    `json:"node"`
	Shares       uint32    `json:"shares"`
	PeerEndpoint string    `json:"peer_endpoint"`
	RDMANICs     []RDMANIC `json:"rdma_nics"`
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
	RDMANICs      []RDMANIC    `json:"rdma_nics"`
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
	BlockDevices     string       `json:"block_devices,omitempty"`
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

func validRDMANIC(r RDMANIC) bool {
	if r.Device == "" || !utf8.ValidString(r.Device) || strings.ContainsAny(r.Device, "\x00\r\n") || r.Port == 0 {
		return false
	}

	if r.GID != "" {
		if len(r.GID) != 32 || strings.ToLower(r.GID) != r.GID {
			return false
		}

		if _, err := hex.DecodeString(r.GID); err != nil {
			return false
		}
	}

	return true
}

func validateRDMANICs(nics []RDMANIC) error {
	if len(nics) > MaxRDMANICs {
		return TooLarge
	}

	type physical struct {
		device string
		port   uint8
	}

	seen := map[physical]bool{}

	for _, nic := range nics {
		key := physical{nic.Device, nic.Port}
		if !validRDMANIC(nic) || seen[key] {
			return InvalidRequest
		}

		seen[key] = true
	}

	return nil
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

	if err := validateRDMANICs(v.RDMANICs); err != nil {
		return err
	}

	remaining := MaxBootstrapBytes
	for _, nic := range v.RDMANICs {
		if len(nic.Device) > remaining {
			return TooLarge
		}

		remaining -= len(nic.Device)
	}

	if _, err := x509.ParseCertificateRequest(v.CSRDER); err != nil {
		return InvalidRequest
	}

	// The validated version is 1 and UUIDs are unescaped ASCII. Only padded
	// base64 contributes variable framing size; no encoder newline is on the wire.
	nics, err := encode(CanonicalRDMANICs(v.RDMANICs), MaxBootstrapBytes)
	if err != nil {
		return err
	}

	const framing = len(`{"shares":,"rdma_nics":,"schema_version":1,"cluster":"","enrollment":"","csr_der":""}`)
	if framing+len(nics)+len(strconv.FormatUint(uint64(v.Shares), 10))+len(v.Cluster)+len(v.Enrollment)+base64.StdEncoding.EncodedLen(len(v.CSRDER)) > MaxBootstrapBytes {
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
		if !validDNSLabel(label) {
			return "", "", InvalidRequest
		}
	}

	client = "/run/racer/" + name + "/client/socket"

	origin = "/run/racer/" + name + "/origin/socket"
	if len(client) > 107 || len(origin) > 107 {
		return "", "", InvalidRequest
	}

	return client, origin, nil
}

func validDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		return false
	}

	for i, c := range []byte(label) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}

		if c != '-' || i == 0 || i == len(label)-1 {
			return false
		}
	}

	return true
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

	if err := checkPublicationSize(v); err != nil {
		return err
	}

	if err := validateMembers(v.Members); err != nil {
		return err
	}

	return validateCaches(v.Caches)
}

func checkPublicationSize(v Publication) error {
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

		for _, r := range m.RDMANICs {
			if !consume(len(r.Device) + len(r.GID) + 1) {
				return TooLarge
			}
		}
	}

	for _, c := range v.Caches {
		if !consume(len(c.ID)) || !consume(len(c.Name)) || !consume(len(c.ClientSocket)) || !consume(len(c.OriginSocket)) {
			return TooLarge
		}
	}

	return nil
}

func validateMembers(members []Member) error {
	nodes := map[NodeID]bool{}
	for _, m := range members {
		if !ValidUUID(string(m.Node)) || nodes[m.Node] || m.Shares == 0 || len(validation.IsValidLabelValue(m.Site)) != 0 {
			return InvalidRequest
		}

		nodes[m.Node] = true

		ap, err := netip.ParseAddrPort(m.PeerEndpoint)
		if err != nil || ap.Port() == 0 || ap.Addr().Zone() != "" {
			return InvalidRequest
		}

		if err := validateRDMANICs(m.RDMANICs); err != nil {
			return err
		}
	}

	return nil
}

func validateCaches(caches []CacheDefinition) error {
	ids := map[CacheID]bool{}

	names := map[string]bool{}
	for _, c := range caches {
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

	return validateCacheKeys(v.CacheKeys, v.Generation)
}

func validateCacheKeys(keys []CacheKey, generation Generation) error {
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

	for _, k := range keys {
		if _, err := NewCacheKey(k.Key, k.State, k.material); err != nil {
			return err
		}

		if binary.BigEndian.Uint64(k.Key.ID[4:12]) > uint64(generation) {
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

// Bounded protocol codecs.

// DecodeBootstrap bounds the entire document before allocating decoded state.
func DecodeBootstrap(r io.Reader) (BootstrapRequest, error) {
	var v BootstrapRequest
	if err := decode(r, MaxBootstrapBytes, &v); err != nil {
		return BootstrapRequest{}, err
	}

	if err := ValidateBootstrapRequest(v); err != nil {
		return BootstrapRequest{}, err
	}

	v.RDMANICs = CanonicalRDMANICs(v.RDMANICs)

	return v, nil
}

func EncodeBootstrap(v BootstrapResponse) ([]byte, error) {
	if err := validateBootstrapResponse(v); err != nil {
		return nil, err
	}

	return encode(v, MaxBootstrapBytes)
}

func EncodeBootstrapRequest(v BootstrapRequest) ([]byte, error) {
	if err := ValidateBootstrapRequest(v); err != nil {
		return nil, err
	}

	v.RDMANICs = CanonicalRDMANICs(v.RDMANICs)

	return encode(v, MaxBootstrapBytes)
}

func DecodeBootstrapResponse(r io.Reader) (BootstrapResponse, error) {
	var v BootstrapResponse
	if err := decode(r, MaxBootstrapBytes, &v); err != nil {
		return BootstrapResponse{}, err
	}

	if err := validateBootstrapResponse(v); err != nil {
		return BootstrapResponse{}, err
	}

	return v, nil
}

// DecodeRDMANICs strictly decodes a bounded NIC annotation and canonicalizes it.
func DecodeRDMANICs(r io.Reader) ([]RDMANIC, error) {
	var nics []RDMANIC
	if err := decode(r, 256*1024, &nics); err != nil {
		return nil, err
	}

	if err := validateRDMANICs(nics); err != nil {
		return nil, err
	}

	return CanonicalRDMANICs(nics), nil
}

// DecodeAdmittedMember validates current restart hints with the strict wire
// shape, including required arrays and rejection of duplicate fields.
func DecodeAdmittedMember(r io.Reader) (Member, error) {
	var member Member
	if err := decode(r, MaxBootstrapBytes, &member); err != nil {
		return Member{}, err
	}

	probe := Publication{SchemaVersion: SchemaVersion, Cluster: ClusterID(member.Node), Sequence: 1, MembershipVersion: 1, Members: []Member{member}}
	if err := validatePublication(probe, true); err != nil {
		return Member{}, err
	}

	member.RDMANICs = CanonicalRDMANICs(member.RDMANICs)

	return member, nil
}

type bundleJSON struct {
	SchemaVersion  uint32     `json:"schema_version"`
	Cluster        ClusterID  `json:"cluster"`
	Generation     Generation `json:"generation,string"`
	PeerTrustRoots [][]byte   `json:"peer_trust_roots"`
	CacheKeys      []keyJSON  `json:"cache_keys"`
}

type keyJSON struct {
	Cache    CacheID    `json:"cache"`
	ID       []byte     `json:"id"`
	Purpose  KeyPurpose `json:"purpose"`
	State    KeyState   `json:"state"`
	Material []byte     `json:"material"`
}

func DecodeBundle(r io.Reader) (KeyringBundle, error) {
	var raw bundleJSON
	if err := decode(r, MaxBundleBytes, &raw); err != nil {
		return KeyringBundle{}, err
	}

	v := KeyringBundle{SchemaVersion: raw.SchemaVersion, Cluster: raw.Cluster, Generation: raw.Generation, PeerTrustRoots: raw.PeerTrustRoots, CacheKeys: make([]CacheKey, 0, len(raw.CacheKeys))}
	for _, k := range raw.CacheKeys {
		if len(k.Material) != 32 {
			return KeyringBundle{}, InvalidRequest
		}

		key, err := NewCacheKey(CacheKeyRef{Cache: k.Cache, ID: k.ID, Purpose: k.Purpose}, k.State, [32]byte(k.Material))
		if err != nil {
			return KeyringBundle{}, err
		}

		v.CacheKeys = append(v.CacheKeys, key)
	}

	if err := validateBundle(v); err != nil {
		return KeyringBundle{}, err
	}

	return v, nil
}

func EncodeBundle(v KeyringBundle) ([]byte, error) {
	if err := validateBundle(v); err != nil {
		return nil, err
	}

	raw := bundleJSON{SchemaVersion: v.SchemaVersion, Cluster: v.Cluster, Generation: v.Generation, PeerTrustRoots: v.PeerTrustRoots, CacheKeys: make([]keyJSON, 0, len(v.CacheKeys))}
	for _, k := range v.CacheKeys {
		raw.CacheKeys = append(raw.CacheKeys, keyJSON{Cache: k.Key.Cache, ID: k.Key.ID, Purpose: k.Key.Purpose, State: k.State, Material: k.material[:]})
	}

	return encode(raw, MaxBundleBytes)
}

func DecodeError(r io.Reader) (ErrorResponse, error) {
	var v ErrorResponse
	if err := decode(r, MaxBootstrapBytes, &v); err != nil {
		return ErrorResponse{}, err
	}

	if !validError(v.Code) {
		return ErrorResponse{}, InvalidRequest
	}

	return v, nil
}

func EncodeError(v ErrorResponse) ([]byte, error) {
	if !validError(v.Code) {
		return nil, InvalidRequest
	}

	return encode(v, MaxBootstrapBytes)
}

// limitedWriter bounds encoder output too, including base64 expansion and escaping.
type limitedWriter struct {
	bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.Len() {
		return 0, TooLarge
	}

	return w.Buffer.Write(p)
}

func encode(v any, limit int) ([]byte, error) {
	w := &limitedWriter{limit: limit + 1} // Encoder adds a newline that is not on the wire.
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)

	if err := e.Encode(v); err != nil {
		return nil, TooLarge
	}

	return bytes.TrimSuffix(w.Bytes(), []byte{'\n'}), nil
}

func decode(r io.Reader, limit int, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return InvalidRequest
	}

	if len(b) > limit {
		return TooLarge
	}

	if !utf8.Valid(b) || !validSurrogates(b) {
		return InvalidRequest
	}

	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()

	if err := checkValue(d, reflect.TypeOf(v).Elem(), false, 0); err != nil {
		return err
	}

	if _, err = d.Token(); err != io.EOF {
		return InvalidRequest
	}
	// The original bytes are safe only after duplicate, exact-name, shape, and
	// primitive checks above. No generic JSON tree is retained by validation.
	if err = json.Unmarshal(b, v); err != nil {
		return InvalidRequest
	}

	return nil
}

// checkValue validates against the wire type while consuming tokens. Reject
// wrong shapes before descending and oversized arrays before their next element;
// never accumulate input-sized maps or slices just to validate the document.
func checkValue(d *json.Decoder, t reflect.Type, quoted bool, depth int) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	v, err := d.Token()
	if err != nil {
		return InvalidRequest
	}

	if _, container := v.(json.Delim); container && depth >= 64 {
		return InvalidRequest
	}

	switch t.Kind() {
	case reflect.Struct:
		if v != json.Delim('{') {
			return InvalidRequest
		}

		return checkObject(d, t, depth)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return checkPrimitive(v, t, quoted)
		}

		if v != json.Delim('[') {
			return InvalidRequest
		}

		return checkArray(d, t.Elem(), depth)
	default:
		return checkPrimitive(v, t, quoted)
	}
}

func checkArray(d *json.Decoder, element reflect.Type, depth int) error {
	for count := 0; d.More(); count++ {
		if (element == reflect.TypeFor[Member]() && count >= MaxMembers) ||
			(element == reflect.TypeFor[RDMANIC]() && count >= MaxRDMANICs) {
			return TooLarge
		}

		if err := checkValue(d, element, false, depth+1); err != nil {
			return err
		}
	}

	return checkEnd(d, ']')
}

func checkEnd(d *json.Decoder, want json.Delim) error {
	if end, err := d.Token(); err != nil || end != want {
		return InvalidRequest
	}

	return nil
}

func fieldIndex(t reflect.Type, key any) int {
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if key == name {
			return i
		}
	}

	return -1
}

func checkField(d *json.Decoder, t reflect.Type, index, depth int) error {
	field := t.Field(index)
	name, option, _ := strings.Cut(field.Tag.Get("json"), ",")
	// An omitted GID is valid, but an explicitly empty GID is not.
	if t == reflect.TypeFor[RDMANIC]() && name == "gid" {
		value, err := d.Token()

		text, ok := value.(string)
		if err != nil || !ok || text == "" {
			return InvalidRequest
		}

		return nil
	}

	return checkValue(d, field.Type, option == "string", depth+1)
}

func checkObject(d *json.Decoder, t reflect.Type, depth int) error {
	// Tracking is sized by the schema, not by attacker-supplied field names.
	seen := make([]bool, t.NumField())

	for d.More() {
		key, err := d.Token()
		if err != nil {
			return InvalidRequest
		}

		index := fieldIndex(t, key)
		if index < 0 || seen[index] {
			return InvalidRequest
		}

		seen[index] = true
		if err := checkField(d, t, index, depth); err != nil {
			return err
		}
	}

	if err := checkEnd(d, '}'); err != nil {
		return err
	}

	for i, present := range seen {
		_, option, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if !present && option != "omitempty" {
			return InvalidRequest
		}
	}

	return nil
}

// encoding/json replaces unpaired UTF-16 surrogates with U+FFFD. Reject that
// lossy conversion so Go and Rust agree on both accepted text and hashes.
func validSurrogates(b []byte) bool {
	quoted := false

	for i := 0; i < len(b); i++ {
		if b[i] == '"' {
			quoted = !quoted
			continue
		}

		if !quoted || b[i] != '\\' {
			continue
		}

		i++
		if i >= len(b) {
			return false
		}

		if b[i] != 'u' {
			continue
		}

		end, ok := unicodeEscapeEnd(b, i)
		if !ok {
			return false
		}

		i = end
	}

	return true
}

// i points at the u in an escape; the result includes a required surrogate pair.
func unicodeEscapeEnd(b []byte, i int) (int, bool) {
	if i+4 >= len(b) {
		return i, false
	}

	n, err := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
	if err != nil || n >= 0xdc00 && n <= 0xdfff {
		return i, false
	}

	i += 4
	if n < 0xd800 || n > 0xdbff {
		return i, true
	}

	if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
		return i, false
	}

	n, err = strconv.ParseUint(string(b[i+3:i+7]), 16, 16)

	return i + 6, err == nil && n >= 0xdc00 && n <= 0xdfff
}

// checkPrimitive rejects null and noncanonical encodings before typed decoding.
func checkPrimitive(v any, t reflect.Type, quoted bool) error {
	switch t.Kind() {
	case reflect.Slice:
		s, ok := v.(string)
		if !ok || t.Elem().Kind() != reflect.Uint8 {
			return InvalidRequest
		}

		b, err := base64.StdEncoding.Strict().DecodeString(s)
		if err != nil || base64.StdEncoding.EncodeToString(b) != s {
			return InvalidRequest
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			return InvalidRequest
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			return InvalidRequest
		}
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return checkUnsigned(v, t.Bits(), quoted)
	default:
		return InvalidRequest
	}

	return nil
}

func checkUnsigned(v any, bits int, quoted bool) error {
	var text string

	if quoted {
		var ok bool

		text, ok = v.(string)
		if !ok {
			return InvalidRequest
		}
	} else {
		n, ok := v.(json.Number)
		if !ok {
			return InvalidRequest
		}

		text = string(n)
	}

	n, err := strconv.ParseUint(text, 10, bits)
	if err != nil || strconv.FormatUint(n, 10) != text {
		return InvalidRequest
	}

	return nil
}

// Canonical publications and counter-free hashes.

// EncodePublication sorts copies of the input collections, never caller state.
func EncodePublication(v Publication) ([]byte, error) {
	c, err := newCanonicalCandidate(v, true)
	if err != nil {
		return nil, err
	}

	return c.EncodePublication(v.Sequence, v.MembershipVersion)
}

func DecodePublication(r io.Reader) (Publication, error) {
	var v Publication
	if err := decode(r, MaxPublicationBytes, &v); err != nil {
		return Publication{}, err
	}

	if err := validatePublication(v, true); err != nil {
		return Publication{}, err
	}

	return canonicalPublication(v), nil
}

func canonicalPublication(v Publication) Publication {
	v.Members = append([]Member{}, v.Members...)

	v.Caches = append([]CacheDefinition{}, v.Caches...)
	for i := range v.Members {
		v.Members[i].RDMANICs = CanonicalRDMANICs(v.Members[i].RDMANICs)
	}

	slices.SortFunc(v.Members, func(a, b Member) int { return cmp.Compare(a.Node, b.Node) })
	slices.SortFunc(v.Caches, func(a, b CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return v
}

// CanonicalRDMANICs copies NICs, including nested pointers, in rail/device/port order.
func CanonicalRDMANICs(nics []RDMANIC) []RDMANIC {
	nics = append([]RDMANIC{}, nics...)
	for i := range nics {
		if n := nics[i].NUMANode; n != nil {
			value := *n
			nics[i].NUMANode = &value
		}
	}

	slices.SortFunc(nics, func(a, b RDMANIC) int {
		if n := cmp.Compare(a.Rail, b.Rail); n != 0 {
			return n
		}

		if n := cmp.Compare(a.Device, b.Device); n != 0 {
			return n
		}

		return cmp.Compare(a.Port, b.Port)
	})

	return nics
}

// CanonicalCandidate owns validated, sorted publication content, including nested
// pointers. Its private state can be reused for hashing and encoding after counter
// assignment without retaining caller-owned mutable state. The zero value is invalid.
type CanonicalCandidate struct {
	publication Publication
}

// NewCanonicalCandidate validates and copies content once. Input counters are
// ignored; final encoding checks the assigned counters and complete byte bound.
func NewCanonicalCandidate(v Publication) (CanonicalCandidate, error) {
	return newCanonicalCandidate(v, false)
}

func newCanonicalCandidate(v Publication, counters bool) (CanonicalCandidate, error) {
	if err := validatePublication(v, counters); err != nil {
		return CanonicalCandidate{}, err
	}

	return CanonicalCandidate{publication: canonicalPublication(v)}, nil
}

// EncodePublication encodes the candidate with nonzero counters. It does not
// mutate the candidate, and each call returns independently owned bytes.
func (c CanonicalCandidate) EncodePublication(sequence Sequence, membership MembershipVersion) ([]byte, error) {
	v := c.publication
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return nil, err
	}

	if sequence == 0 || membership == 0 {
		return nil, InvalidRequest
	}

	v.Sequence, v.MembershipVersion = sequence, membership

	return encode(v, MaxPublicationBytes)
}

// canonicalContent returns counter-free canonical JSON for durable version CAS.
// The membership document includes schema and cluster, and every member input.
// Input counters may be zero because callers hash candidates before assigning them.
func (c CanonicalCandidate) canonicalContent() (content, membership []byte, err error) {
	v := c.publication
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return nil, nil, err
	}

	m := struct {
		SchemaVersion uint32    `json:"schema_version"`
		Cluster       ClusterID `json:"cluster"`
		Members       []Member  `json:"members"`
	}{v.SchemaVersion, v.Cluster, v.Members}
	p := struct {
		SchemaVersion uint32            `json:"schema_version"`
		Cluster       ClusterID         `json:"cluster"`
		Members       []Member          `json:"members"`
		Caches        []CacheDefinition `json:"caches"`
	}{v.SchemaVersion, v.Cluster, v.Members, v.Caches}

	content, err = encode(p, MaxPublicationBytes)
	if err != nil {
		return nil, nil, err
	}

	membership, err = encode(m, MaxPublicationBytes)

	return content, membership, err
}

// ContentHashes returns lowercase SHA-256 hex, suitable for VersionRecord fields.
func ContentHashes(v Publication) (content, membership string, err error) {
	c, err := NewCanonicalCandidate(v)
	if err != nil {
		return "", "", err
	}

	return c.ContentHashes()
}

// ContentHashes returns the same counter-free hashes as ContentHashes without
// repeating validation, copying, or sorting of the candidate's collections.
func (c CanonicalCandidate) ContentHashes() (content, membership string, err error) {
	p, m, err := c.canonicalContent()
	if err != nil {
		return "", "", err
	}

	ph, mh := sha256.Sum256(p), sha256.Sum256(m)

	return hex.EncodeToString(ph[:]), hex.EncodeToString(mh[:]), nil
}

// Snapshot deltas use the same validation and hashes as full publications.

const (
	DeltaHeader   = "X-Racer-Delta-Base"
	MaxDeltaBytes = 4 * 1024 * 1024
)

// Delta is transported only on the authenticated snapshot endpoint. Hashes use
// ContentHashes, not a second canonical representation or placement authority.
type Delta struct {
	DeltaVersion      uint32            `json:"delta_version"`
	Cluster           ClusterID         `json:"cluster"`
	BaseSequence      Sequence          `json:"base_sequence,string"`
	BaseHash          string            `json:"base_hash"`
	Sequence          Sequence          `json:"sequence,string"`
	MembershipVersion MembershipVersion `json:"membership_version,string"`
	ContentHash       string            `json:"content_hash"`
	UpsertMembers     []Member          `json:"upsert_members"`
	RemoveMembers     []NodeID          `json:"remove_members"`
	Caches            []CacheDefinition `json:"caches"`
}

func EncodeDelta(base, next Publication) ([]byte, error) {
	b, err := newCanonicalCandidate(base, true)
	if err != nil {
		return nil, err
	}

	n, err := newCanonicalCandidate(next, true)
	if err != nil {
		return nil, err
	}

	if base.Cluster != next.Cluster || next.Sequence <= base.Sequence {
		return nil, Conflict
	}

	base, next = b.publication, n.publication

	bh, _, err := b.ContentHashes()
	if err != nil {
		return nil, err
	}

	nh, _, err := n.ContentHashes()
	if err != nil {
		return nil, err
	}

	d := Delta{DeltaVersion: 1, Cluster: next.Cluster, BaseSequence: base.Sequence, BaseHash: bh, Sequence: next.Sequence, MembershipVersion: next.MembershipVersion, ContentHash: nh, UpsertMembers: []Member{}, RemoveMembers: []NodeID{}, Caches: next.Caches}

	old := make(map[NodeID]Member, len(base.Members))
	for _, m := range base.Members {
		old[m.Node] = m
	}

	for _, m := range next.Members {
		if previous, ok := old[m.Node]; !ok || !reflect.DeepEqual(previous, m) {
			d.UpsertMembers = append(d.UpsertMembers, m)
		}

		delete(old, m.Node)
	}

	for _, m := range base.Members {
		if _, ok := old[m.Node]; ok {
			d.RemoveMembers = append(d.RemoveMembers, m.Node)
		}
	}

	return encode(d, MaxDeltaBytes)
}

func ApplyDelta(base Publication, reader io.Reader) (Publication, error) {
	var d Delta
	if err := decode(reader, MaxDeltaBytes, &d); err != nil {
		return Publication{}, err
	}

	hash, _, err := ContentHashes(base)
	if err != nil {
		return Publication{}, err
	}

	if d.DeltaVersion != 1 || d.Cluster != base.Cluster || d.BaseSequence != base.Sequence || d.BaseHash != hash || d.Sequence <= base.Sequence || d.MembershipVersion < base.MembershipVersion {
		return Publication{}, Conflict
	}

	members, err := applyMemberChanges(base.Members, d)
	if err != nil {
		return Publication{}, err
	}

	next := Publication{SchemaVersion: SchemaVersion, Cluster: d.Cluster, Sequence: d.Sequence, MembershipVersion: d.MembershipVersion, Caches: d.Caches, Members: members}

	candidate, err := newCanonicalCandidate(next, true)
	if err != nil {
		return Publication{}, err
	}

	hash, _, err = candidate.ContentHashes()
	if err != nil {
		return Publication{}, err
	}

	if hash != d.ContentHash {
		return Publication{}, Conflict
	}

	return candidate.publication, nil
}

func applyMemberChanges(base []Member, d Delta) ([]Member, error) {
	members := make(map[NodeID]Member, len(base))
	for _, m := range base {
		members[m.Node] = m
	}
	// A node may occur only once across both change lists.
	seen := make(map[NodeID]bool)
	for _, id := range d.RemoveMembers {
		if _, ok := members[id]; !ok || seen[id] {
			return nil, InvalidRequest
		}

		seen[id] = true
		delete(members, id)
	}

	for _, m := range d.UpsertMembers {
		if seen[m.Node] {
			return nil, InvalidRequest
		}

		seen[m.Node] = true
		members[m.Node] = m
	}

	next := make([]Member, 0, len(members))
	for _, m := range members {
		next = append(next, m)
	}

	return next, nil
}
