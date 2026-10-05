// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

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

// DecodeAdmittedMember accepts pre-NIC persisted records only for restart
// continuity. Legacy rails never become NICs. New records use the strict wire
// shape, including required arrays and rejection of duplicate fields.
func DecodeAdmittedMember(r io.Reader) (Member, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBootstrapBytes+1))
	if err != nil {
		return Member{}, InvalidRequest
	}

	if len(b) > MaxBootstrapBytes {
		return Member{}, TooLarge
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return Member{}, InvalidRequest
	}

	var member Member
	if _, present := fields["rdma_nics"]; present {
		if err := decode(bytes.NewReader(b), MaxBootstrapBytes, &member); err != nil {
			return Member{}, err
		}
	} else {
		var legacy struct {
			Node         NodeID `json:"node"`
			Shares       uint32 `json:"shares"`
			PeerEndpoint string `json:"peer_endpoint"`
			Rails        []struct {
				Rail     uint16  `json:"rail"`
				Fabric   string  `json:"fabric"`
				NUMANode *uint32 `json:"numa_node,omitempty"`
			} `json:"rails"`
			AlignmentEnabled bool   `json:"alignment_enabled"`
			Site             string `json:"site,omitempty"`
		}
		if err := decode(bytes.NewReader(b), MaxBootstrapBytes, &legacy); err != nil {
			return Member{}, err
		}

		member = Member{Node: legacy.Node, Shares: legacy.Shares, PeerEndpoint: legacy.PeerEndpoint, Site: legacy.Site, RDMANICs: []RDMANIC{}}
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

		for count := 0; d.More(); count++ {
			if (t.Elem() == reflect.TypeFor[Member]() && count >= MaxMembers) ||
				(t.Elem() == reflect.TypeFor[RDMANIC]() && count >= MaxRDMANICs) {
				return TooLarge
			}

			if err := checkValue(d, t.Elem(), false, depth+1); err != nil {
				return err
			}
		}

		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return InvalidRequest
		}

		return nil
	default:
		return checkPrimitive(v, t, quoted)
	}
}

func checkObject(d *json.Decoder, t reflect.Type, depth int) error {
	// Tracking is sized by the schema, not by attacker-supplied field names.
	seen := make([]bool, t.NumField())

	for d.More() {
		key, err := d.Token()
		if err != nil {
			return InvalidRequest
		}

		index := -1

		for i := range t.NumField() {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			if key == name {
				index = i
				break
			}
		}

		if index < 0 || seen[index] {
			return InvalidRequest
		}

		seen[index] = true
		field := t.Field(index)
		_, option, _ := strings.Cut(field.Tag.Get("json"), ",")

		if t == reflect.TypeFor[RDMANIC]() && key == "gid" {
			value, err := d.Token()

			text, ok := value.(string)
			if err != nil || !ok || text == "" {
				return InvalidRequest
			}
		} else if err := checkValue(d, field.Type, option == "string", depth+1); err != nil {
			return err
		}
	}

	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return InvalidRequest
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

		if i+4 >= len(b) {
			return false
		}

		n, err := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}

		i += 4

		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}

		if n < 0xd800 || n > 0xdbff {
			continue
		}

		if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
			return false
		}

		n, err = strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
		if err != nil || n < 0xdc00 || n > 0xdfff {
			return false
		}

		i += 6
	}

	return true
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
		var s string

		if quoted {
			var ok bool

			s, ok = v.(string)
			if !ok {
				return InvalidRequest
			}
		} else {
			n, ok := v.(json.Number)
			if !ok {
				return InvalidRequest
			}

			s = string(n)
		}

		n, err := strconv.ParseUint(s, 10, t.Bits())
		if err != nil || strconv.FormatUint(n, 10) != s {
			return InvalidRequest
		}
	default:
		return InvalidRequest
	}

	return nil
}
