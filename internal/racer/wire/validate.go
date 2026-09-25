// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"crypto/x509"
	"net/netip"
	"strings"
	"unicode/utf8"
)

func validUUID(s string) bool {
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

// ValidUUID checks canonical Kubernetes identities before constructing wire state.
func ValidUUID(s string) bool { return validUUID(s) }

func validRail(r Rail) bool {
	return r.Fabric != "" && utf8.ValidString(r.Fabric) && !strings.ContainsAny(r.Fabric, "\x00\r\n")
}

func validateHeader(version uint32, cluster ClusterID) error {
	if version != SchemaVersion {
		return UnsupportedVersion
	}

	if !validUUID(string(cluster)) {
		return InvalidRequest
	}

	return nil
}

func validateBootstrapRequest(v BootstrapRequest) error {
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return err
	}

	if !validUUID(string(v.Enrollment)) {
		return InvalidRequest
	}

	if len(v.CSRDER) > MaxBootstrapBytes {
		return TooLarge
	}

	if _, err := x509.ParseCertificateRequest(v.CSRDER); err != nil {
		return InvalidRequest
	}

	return nil
}

func validateBootstrapResponse(v BootstrapResponse) error {
	if err := validateHeader(v.SchemaVersion, v.Cluster); err != nil {
		return err
	}

	if !validUUID(string(v.Node)) || !validUUID(string(v.Enrollment)) {
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
		if !consume(len(m.Node)) || !consume(len(m.PeerEndpoint)) {
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
		if !validUUID(string(m.Node)) || nodes[m.Node] || m.Shares == 0 {
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
		if !validUUID(string(c.ID)) || ids[c.ID] || names[c.Name] || c.SocketMode > 0o777 {
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
	active := map[scope]int{}

	for _, k := range v.CacheKeys {
		if _, err := NewCacheKey(k.Key, k.State, k.material); err != nil {
			return err
		}

		s := scope{k.Key.Cache, k.Key.Purpose}

		id := identity{s, string(k.Key.ID)}
		if seen[id] {
			return InvalidRequest
		}

		seen[id] = true

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

// EqualMaterial compares private key bytes without exposing them to consumers.
// It is intended for bundle replay validation, not authentication.
func (k CacheKey) EqualMaterial(other CacheKey) bool {
	return bytes.Equal(k.material[:], other.material[:])
}
