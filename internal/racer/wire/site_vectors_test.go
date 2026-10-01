// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type siteVector struct {
	Name           string `json:"name"`
	Site           string `json:"site"`
	Publication    string `json:"publication"`
	Content        string `json:"content"`
	Membership     string `json:"membership"`
	ContentHash    string `json:"content_hash"`
	MembershipHash string `json:"membership_hash"`
	Delta          string `json:"delta,omitempty"`
}

// TestGenerateSharedSiteVectors is opt-in so ordinary tests never rewrite goldens.
// Regenerate with RACER_UPDATE_SITE_VECTORS=1 go test -timeout=5m ./internal/racer/wire -run '^TestGenerateSharedSiteVectors$'.
// Rust independently consumes the same output in shared_site_vectors.
func TestGenerateSharedSiteVectors(t *testing.T) {
	if os.Getenv("RACER_UPDATE_SITE_VECTORS") != "1" {
		t.Skip("set RACER_UPDATE_SITE_VECTORS=1 to regenerate shared Site vectors")
	}

	regenerateSharedVectors(t)
	p, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	var (
		previous Publication
		vectors  []siteVector
	)

	for i, step := range []struct{ name, site string }{
		{"absent", ""},
		{"added", "Site_1.west-2"},
		{"changed", "Site_2.east-1"},
		{"removed", ""},
	} {
		p.Sequence, p.MembershipVersion = Sequence(i+1), MembershipVersion(i+1)
		p.Members[0].Site = step.site
		v := siteVector{Name: step.name, Site: step.site}
		publication, err := EncodePublication(p)
		require.NoError(t, err)

		v.Publication = string(publication)
		candidate, err := NewCanonicalCandidate(p)
		require.NoError(t, err)
		content, membership, err := candidate.canonicalContent()
		require.NoError(t, err)

		v.Content, v.Membership = string(content), string(membership)
		v.ContentHash, v.MembershipHash, err = candidate.ContentHashes()
		require.NoError(t, err)

		if i > 0 {
			delta, err := EncodeDelta(previous, p)
			require.NoError(t, err)

			v.Delta = string(delta)
		}

		vectors = append(vectors, v)
		previous = canonicalPublication(p)
	}

	encoded, err := json.MarshalIndent(vectors, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile("testdata/site-vectors.json", append(encoded, '\n'), 0o644))
}

func TestSharedSiteVectors(t *testing.T) {
	var vectors []siteVector
	require.NoError(t, json.Unmarshal(fixture(t, "site-vectors.json"), &vectors))
	require.Len(t, vectors, 4)

	legacy, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	var previous Publication

	for i, step := range []struct{ name, site string }{
		{"absent", ""},
		{"added", "Site_1.west-2"},
		{"changed", "Site_2.east-1"},
		{"removed", ""},
	} {
		v := vectors[i]

		t.Run(step.name, func(t *testing.T) {
			require.Equal(t, step.name, v.Name)
			require.Equal(t, step.site, v.Site)
			p, err := DecodePublication(bytes.NewBufferString(v.Publication))
			require.NoError(t, err)

			want := canonicalPublication(legacy)
			want.Sequence, want.MembershipVersion = Sequence(i+1), MembershipVersion(i+1)
			want.Members[0].Site = step.site
			require.Equal(t, want, p, "only Site and counters change")
			encoded, err := EncodePublication(p)
			require.NoError(t, err)
			require.Equal(t, []byte(v.Publication), encoded)

			if step.site == "" {
				require.Contains(t, string(encoded), `"site":""`)
			} else {
				require.Contains(t, string(encoded), `"alignment_enabled":true,"site":"`+step.site+`"`)
			}

			candidate, err := NewCanonicalCandidate(p)
			require.NoError(t, err)
			content, membership, err := candidate.canonicalContent()
			require.NoError(t, err)
			require.Equal(t, []byte(v.Content), content)
			require.Equal(t, []byte(v.Membership), membership)

			ph, mh, err := ContentHashes(p)
			require.NoError(t, err)
			require.Equal(t, v.ContentHash, ph)
			require.Equal(t, v.MembershipHash, mh)

			if i == 0 {
				require.Empty(t, v.Delta)
				return
			}

			require.NotEqual(t, vectors[i-1].ContentHash, ph)
			require.NotEqual(t, vectors[i-1].MembershipHash, mh)

			delta, err := EncodeDelta(previous, p)
			require.NoError(t, err)
			require.Equal(t, []byte(v.Delta), delta)
			applied, err := ApplyDelta(previous, bytes.NewBufferString(v.Delta))
			require.NoError(t, err)
			require.Equal(t, p, applied)

			var d Delta
			require.NoError(t, json.Unmarshal([]byte(v.Delta), &d))
			require.Equal(t, []Member{p.Members[0]}, d.UpsertMembers)
			require.Empty(t, d.RemoveMembers, "removing Site replaces the member, not the node")
			d.UpsertMembers[0].Site = "tampered-site"
			bad, err := json.Marshal(d)
			require.NoError(t, err)
			_, err = ApplyDelta(previous, bytes.NewReader(bad))
			require.ErrorIs(t, err, Conflict, "Site is bound to the target hash")

			wrongBase := canonicalPublication(previous)
			wrongBase.Members[0].Site = "stale-site"
			_, err = ApplyDelta(wrongBase, bytes.NewBufferString(v.Delta))
			require.ErrorIs(t, err, Conflict, "Site is bound to the base hash")
			_, err = ApplyDelta(p, bytes.NewBufferString(v.Delta))
			require.ErrorIs(t, err, Conflict, "replay rejected")
		})

		previous, err = DecodePublication(bytes.NewBufferString(v.Publication))
		require.NoError(t, err)
	}

	require.Equal(t, vectors[0].Content, vectors[3].Content)
	require.Equal(t, vectors[0].Membership, vectors[3].Membership)
	require.Equal(t, vectors[0].ContentHash, vectors[3].ContentHash)
	require.Equal(t, vectors[0].MembershipHash, vectors[3].MembershipHash)
}

// regenerateSharedVectors migrates public fixture inputs through the current
// encoders. No private certificate key or production key material is persisted.
func regenerateSharedVectors(t *testing.T) {
	t.Helper()

	write := func(name string, b []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile("testdata/"+name, append(b, '\n'), 0o644))

		if name == "publication.json" || name == "bootstrap-request.json" || name == "bootstrap-response.json" || name == "bundle.json" {
			require.NoError(t, os.WriteFile("../../../cmd/racer-dataplane/src/control/testdata/"+name, append(b, '\n'), 0o644))
		}
	}

	var p Publication
	require.NoError(t, json.Unmarshal(fixture(t, "publication.json"), &p))
	b, err := json.Marshal(p)
	require.NoError(t, err)
	write("publication.json", b)

	candidate, err := NewCanonicalCandidate(p)
	require.NoError(t, err)
	c, m, err := candidate.canonicalContent()
	require.NoError(t, err)
	write("content.json", c)
	write("membership.json", m)

	ph, mh, err := candidate.ContentHashes()
	require.NoError(t, err)
	b, err = json.Marshal(map[string]string{"content": ph, "membership": mh})
	require.NoError(t, err)
	write("hashes.json", b)

	var request BootstrapRequest
	require.NoError(t, json.Unmarshal(fixture(t, "bootstrap-request.json"), &request))
	request.Shares = DefaultShares
	b, err = EncodeBootstrapRequest(request)
	require.NoError(t, err)
	write("bootstrap-request.json", b)
	write("bootstrap-response.json", fixture(t, "bootstrap-response.json"))

	var bundle bundleJSON
	require.NoError(t, json.Unmarshal(fixture(t, "bundle.json"), &bundle))

	for i := range bundle.CacheKeys {
		id := make([]byte, 16)
		copy(id, "RKG1")
		binary.BigEndian.PutUint64(id[4:12], uint64(i%2+1))
		bundle.CacheKeys[i].ID = id
		// Deterministic public test material must be unique across refs too.
		bundle.CacheKeys[i].Material = bytes.Repeat([]byte{byte(i)}, 32)
	}

	b, err = json.Marshal(bundle)
	require.NoError(t, err)
	_, err = DecodeBundle(bytes.NewReader(b))
	require.NoError(t, err)
	write("bundle.json", b)
}
