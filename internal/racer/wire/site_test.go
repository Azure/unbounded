// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemberSiteWireValidation(t *testing.T) {
	for _, site := range []string{"", "a", "0", "Site_1.a-b", strings.Repeat("a", 63)} {
		t.Run("valid/"+site, func(t *testing.T) {
			v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
			require.NoError(t, err)

			v.Members[0].Site = site
			encoded, err := EncodePublication(v)
			require.NoError(t, err)

			if site == "" {
				require.Contains(t, string(encoded), `"site":""`)
			} else {
				require.Contains(t, string(encoded), `"alignment_enabled":true,"site":"`+site+`"`)
			}

			decoded, err := DecodePublication(bytes.NewReader(encoded))
			require.NoError(t, err)
			require.Equal(t, v, decoded)
		})
	}

	for _, site := range []string{strings.Repeat("a", 64), "-a", "a-", ".a", "a.", "_a", "a_", "a/b", "a b", "a\n", "a\x00", "β", "\xff"} {
		t.Run("invalid/"+site, func(t *testing.T) {
			v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
			require.NoError(t, err)

			v.Members[0].Site = site
			_, err = EncodePublication(v)
			require.ErrorIs(t, err, InvalidRequest)
			_, err = NewCanonicalCandidate(v)
			require.ErrorIs(t, err, InvalidRequest)
			encoded, err := json.Marshal(v)
			require.NoError(t, err)
			_, err = DecodePublication(bytes.NewReader(encoded))
			require.ErrorIs(t, err, InvalidRequest)
		})
	}
}

func TestMemberSiteJSONShape(t *testing.T) {
	for _, value := range []string{`null`, `1`, `true`, `[]`, `{}`, `"a","site":"b"`} {
		raw := strings.Replace(string(fixture(t, "publication.json")), `"alignment_enabled":true`, `"alignment_enabled":true,"site":`+value, 1)
		_, err := DecodePublication(strings.NewReader(raw))
		require.ErrorIs(t, err, InvalidRequest, "%s", value)
	}

	raw := string(fixture(t, "publication.json"))
	v, err := DecodePublication(strings.NewReader(raw))
	require.NoError(t, err)
	encoded, err := EncodePublication(v)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"site":""`)
}

func TestMemberSiteDeltaValidation(t *testing.T) {
	base, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	base.Sequence, base.MembershipVersion = 1, 1
	next := canonicalPublication(base)
	next.Sequence, next.MembershipVersion = 2, 2
	next.Members[0].Site = "site-a"
	encoded, err := EncodeDelta(base, next)
	require.NoError(t, err)
	applied, err := ApplyDelta(base, bytes.NewReader(encoded))
	require.NoError(t, err)
	require.Equal(t, next, applied)

	for _, site := range []string{"-invalid", "site-b"} {
		bad := strings.Replace(string(encoded), `"site":"site-a"`, `"site":"`+site+`"`, 1)

		_, err := ApplyDelta(base, strings.NewReader(bad))
		if site == "-invalid" {
			require.ErrorIs(t, err, InvalidRequest)
		} else {
			require.ErrorIs(t, err, Conflict)
		}
	}
}
