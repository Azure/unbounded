// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIgnitionSpecVersionIsPinned guards the one constant an operator cannot
// recover from being wrong.
//
// Ignition refuses a config whose version it does not implement, and it refuses
// it on first boot with no shell and no agent yet installed. There is nothing on
// the host to report the mismatch, so the failure presents as a machine that
// provisioned into nothing.
func TestIgnitionSpecVersionIsPinned(t *testing.T) {
	t.Parallel()

	require.Equal(t, "3.4.0", ignitionSpecVersion)
}

// TestIgnitionDataURLRoundTrips covers how inline file contents reach the host.
// Ignition reads them from a data URL, so anything lost in the encoding is lost
// silently: the file appears, with the wrong bytes in it.
func TestIgnitionDataURLRoundTrips(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"",
		"plain",
		"{\n  \"MachineName\": \"kube1\"\n}\n",
		"trailing newline\n",
		"unicode: \u00e9\u00e8\u00ea and emoji bytes",
		"null\x00byte",
	} {
		t.Run(strings.SplitN(content, "\n", 2)[0], func(t *testing.T) {
			t.Parallel()

			url := ignitionDataURL(content)
			require.True(t, strings.HasPrefix(url, "data:;base64,"), "got %q", url)

			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:;base64,"))
			require.NoError(t, err)
			require.Equal(t, content, string(decoded))
		})
	}
}

// TestIgnitionRemoteFetchable pins which sources Ignition can retrieve itself.
//
// This decides where a file lands in the boot. A fetchable source is written
// before dbus starts; anything else has to wait for the agent, which is after.
// Reading it the wrong way round produces a config Ignition rejects, or a file
// that silently arrives too late to be useful.
func TestIgnitionRemoteFetchable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"https://example.test/unbounded-agent", true},
		{"http://example.test/unbounded-agent", true},
		{"tftp://example.test/unbounded-agent", true},
		{"s3://bucket/unbounded-agent", true},
		{"arn:aws:s3:::bucket/unbounded-agent", true},
		{"gs://bucket/unbounded-agent", true},
		{"  https://example.test/spaced  ", true},

		// oci is the one that matters: it is the agent's own artifact scheme,
		// and Ignition has no idea what to do with it.
		{"oci://ghcr.io/azure/unbounded-agent:v1", false},
		{"file:///tmp/unbounded-agent", false},
		{"ftp://example.test/unbounded-agent", false},
		{"/usr/local/bin/unbounded-agent", false},
		{"", false},
		{"://not a url", false},
	} {
		t.Run(tc.source, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, ignitionRemoteFetchable(tc.source))
		})
	}
}

// TestIgnitionHashFromSHA256 covers the digest conversion, including the
// sha256sum shape an operator is most likely to paste in.
func TestIgnitionHashFromSHA256(t *testing.T) {
	t.Parallel()

	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	t.Run("plain digest", func(t *testing.T) {
		t.Parallel()

		got, err := ignitionHashFromSHA256(digest)
		require.NoError(t, err)
		require.Equal(t, "sha256-"+digest, got)
	})

	t.Run("sha256sum output", func(t *testing.T) {
		t.Parallel()

		got, err := ignitionHashFromSHA256(digest + "  unbounded-agent\n")
		require.NoError(t, err)
		require.Equal(t, "sha256-"+digest, got)
	})

	t.Run("uppercase is normalized", func(t *testing.T) {
		t.Parallel()

		got, err := ignitionHashFromSHA256(strings.ToUpper(digest))
		require.NoError(t, err)
		require.Equal(t, "sha256-"+digest, got, "Ignition compares the hash as written")
	})

	for _, tc := range []struct{ name, input string }{
		{"empty", ""},
		{"too short", digest[:63]},
		{"too long", digest + "0"},
		{"non-hex", strings.Replace(digest, "9", "z", 1)},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ignitionHashFromSHA256(tc.input)
			require.Error(t, err, "a malformed digest must fail here, not on the host at first boot")
		})
	}
}

// TestIgnitionConfigOmitsEmptySections pins that the emitted document contains
// only what was asked for.
//
// Ignition validates the whole config before acting on any of it, so an empty
// section serialized as null or [] can reject a config that is otherwise fine,
// again on a host with nothing available to say so.
func TestIgnitionConfigOmitsEmptySections(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(ignitionConfig{Ignition: ignitionVersion{Version: ignitionSpecVersion}})
	require.NoError(t, err)

	require.JSONEq(t, `{"ignition":{"version":"3.4.0"}}`, string(encoded))
	require.NotContains(t, string(encoded), "storage")
	require.NotContains(t, string(encoded), "systemd")
}

// TestIgnitionFileModesSerializeAsDecimal covers a trap in the format: Ignition
// file modes are decimal integers, and Go's octal literals are easy to read as
// if they were being emitted verbatim.
//
// A mode written as 600 rather than 0o600 is 0o1130 on disk, which for the
// agent config means credentials readable by everyone.
func TestIgnitionFileModesSerializeAsDecimal(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(ignitionFile{
		Path:     "/etc/unbounded/agent/config.json",
		Mode:     ignitionModeConfig,
		Contents: ignitionContents{Source: ignitionDataURL("{}")},
	})
	require.NoError(t, err)

	// 0o600 is 384 decimal. Asserting the number rather than the constant is
	// the point: it is what a reader of the emitted config would see.
	require.Contains(t, string(encoded), `"mode":384`)

	require.Equal(t, 0o600, ignitionModeConfig, "the agent config carries credentials")
	require.Equal(t, 0o755, ignitionModeScript)
	require.Equal(t, 0o755, ignitionModeDir)
}
