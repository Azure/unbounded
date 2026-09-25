// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrUnimplemented = errors.New("racer operation is not implemented")

func Pending(operation string) error {
	return fmt.Errorf("%s: %w", operation, ErrUnimplemented)
}

// Error returns only a protocol code, never input or secret material.
func (c ErrorCode) Error() string { return string(c) }

// DecodeBootstrap bounds the entire document before allocating decoded state.
func DecodeBootstrap(r io.Reader) (BootstrapRequest, error) {
	var v BootstrapRequest
	if err := decode(r, MaxBootstrapBytes, &v); err != nil {
		return BootstrapRequest{}, err
	}

	if err := validateBootstrapRequest(v); err != nil {
		return BootstrapRequest{}, err
	}

	return v, nil
}

func EncodeBootstrap(v BootstrapResponse) ([]byte, error) {
	if err := validateBootstrapResponse(v); err != nil {
		return nil, err
	}

	return encode(v, MaxBootstrapBytes)
}

func EncodeBootstrapRequest(v BootstrapRequest) ([]byte, error) {
	if err := validateBootstrapRequest(v); err != nil {
		return nil, err
	}

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

// EncodePublication sorts copies of the input collections, never caller state.
func EncodePublication(v Publication) ([]byte, error) {
	if err := validatePublication(v, true); err != nil {
		return nil, err
	}

	return encode(canonicalPublication(v), MaxPublicationBytes)
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

// NewCacheKey is the only material ingress besides bounded bundle decoding.
func NewCacheKey(ref CacheKeyRef, state KeyState, material [32]byte) (CacheKey, error) {
	if !validUUID(string(ref.Cache)) || len(ref.ID) != 16 || (ref.Purpose != PageKey && ref.Purpose != OriginCredentialsKey) || (state != PreparedKey && state != ActiveKey && state != RetiringKey) {
		return CacheKey{}, InvalidRequest
	}

	ref.ID = bytes.Clone(ref.ID)

	return CacheKey{Key: ref, State: state, material: material}, nil
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

func validError(c ErrorCode) bool {
	switch c {
	case InvalidRequest, Unauthenticated, Forbidden, Conflict, TooLarge, UnsupportedVersion, Overloaded, Unavailable:
		return true
	default:
		return false
	}
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

	tree, err := readValue(d, 0)
	if err != nil {
		return InvalidRequest
	}

	if _, err = d.Token(); err != io.EOF {
		return InvalidRequest
	}

	if err = checkShape(tree, reflect.TypeOf(v).Elem(), false); err != nil {
		return err
	}
	// Re-encode the exact-name projection. encoding/json otherwise matches unknown
	// fields case-insensitively, potentially overwriting a validated known field.
	b, err = json.Marshal(tree)
	if err != nil {
		return InvalidRequest
	}

	if err = json.Unmarshal(b, v); err != nil {
		return InvalidRequest
	}

	return nil
}

// readValue checks duplicates even in ignored fields and caps nesting. JSON's
// default map decoder silently overwrites duplicates and is not suitable here.
func readValue(d *json.Decoder, depth int) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, InvalidRequest
	}

	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}

	if depth >= 64 {
		return nil, InvalidRequest
	}

	switch delim {
	case '{':
		m := map[string]any{}

		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, InvalidRequest
			}

			s, ok := key.(string)
			if !ok {
				return nil, InvalidRequest
			}

			if _, exists := m[s]; exists {
				return nil, InvalidRequest
			}

			value, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}

			m[s] = value
		}

		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return nil, InvalidRequest
		}

		return m, nil
	case '[':
		a := []any{}

		for d.More() {
			value, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}

			a = append(a, value)
		}

		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return nil, InvalidRequest
		}

		return a, nil
	default:
		return nil, InvalidRequest
	}
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

// checkShape enforces exact field names, required fields, non-null values and
// canonical primitive encodings before the standard decoder builds typed state.
func checkShape(v any, t reflect.Type, quoted bool) error {
	if t.Kind() == reflect.Pointer {
		return checkShape(v, t.Elem(), quoted)
	}

	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return InvalidRequest
		}

		known := map[string]bool{}

		for i := range t.NumField() {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			known[tag[0]] = true

			value, exists := m[tag[0]]
			if !exists && len(tag) > 1 && tag[1] == "omitempty" {
				continue
			}

			if !exists {
				return InvalidRequest
			}

			if err := checkShape(value, f.Type, len(tag) > 1 && tag[1] == "string"); err != nil {
				return err
			}
		}

		for key := range m {
			if !known[key] {
				delete(m, key)
			}
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			s, ok := v.(string)
			if !ok {
				return InvalidRequest
			}

			b, err := base64.StdEncoding.Strict().DecodeString(s)
			if err != nil || base64.StdEncoding.EncodeToString(b) != s {
				return InvalidRequest
			}

			return nil
		}

		a, ok := v.([]any)
		if !ok {
			return InvalidRequest
		}

		if t.Elem() == reflect.TypeFor[Member]() && len(a) > MaxMembers {
			return TooLarge
		}

		for _, x := range a {
			if err := checkShape(x, t.Elem(), false); err != nil {
				return err
			}
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			return InvalidRequest
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			return InvalidRequest
		}
	case reflect.Uint16, reflect.Uint32, reflect.Uint64:
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
