// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// Ignition configuration types, covering only the subset this command emits.
//
// These are hand-written rather than taken from github.com/coreos/ignition to
// avoid a dependency carrying the whole specification for the handful of fields
// used here. The schema version is pinned and asserted by tests.
const ignitionSpecVersion = "3.4.0"

// File modes are serialized as decimal integers in an Ignition config.
const (
	ignitionModeConfig = 0o600
	ignitionModeScript = 0o755
	ignitionModeData   = 0o644
)

type ignitionConfig struct {
	Ignition ignitionVersion  `json:"ignition"`
	Storage  *ignitionStorage `json:"storage,omitempty"`
	Systemd  *ignitionSystemd `json:"systemd,omitempty"`
}

type ignitionVersion struct {
	Version string `json:"version"`
}

type ignitionStorage struct {
	Files []ignitionFile `json:"files,omitempty"`
}

type ignitionFile struct {
	Path      string           `json:"path"`
	Mode      int              `json:"mode,omitempty"`
	Overwrite *bool            `json:"overwrite,omitempty"`
	Contents  ignitionContents `json:"contents"`
}

type ignitionContents struct {
	Source       string                `json:"source"`
	Verification *ignitionVerification `json:"verification,omitempty"`
}

type ignitionVerification struct {
	// Hash is "<algorithm>-<hex>", for example "sha256-abc123...".
	Hash string `json:"hash"`
}

type ignitionSystemd struct {
	Units []ignitionUnit `json:"units,omitempty"`
}

type ignitionUnit struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Contents string `json:"contents,omitempty"`
}

// ignitionDataURL encodes content as a data URL, which is how Ignition carries
// inline file contents.
func ignitionDataURL(content string) string {
	return "data:;base64," + base64.StdEncoding.EncodeToString([]byte(content))
}

// ignitionRemoteFetchable reports whether Ignition can fetch a source itself.
//
// Ignition understands http, https, tftp, s3, arn, gs and data. It does not
// understand oci, which the agent resolves through its own artifact source.
// A source Ignition cannot fetch has to be left to the agent, which means the
// file lands after dbus has already started.
func ignitionRemoteFetchable(source string) bool {
	parsed, err := url.Parse(strings.TrimSpace(source))
	if err != nil {
		return false
	}

	switch parsed.Scheme {
	case "http", "https", "tftp", "s3", "arn", "gs":
		return true
	default:
		return false
	}
}

// ignitionHashFromSHA256 converts a hex digest into the form Ignition expects.
func ignitionHashFromSHA256(hex string) (string, error) {
	trimmed := strings.TrimSpace(hex)

	// Accept a plain digest or the first field of sha256sum output.
	if fields := strings.Fields(trimmed); len(fields) > 0 {
		trimmed = fields[0]
	}

	if len(trimmed) != 64 {
		return "", fmt.Errorf("sha256 digest must be 64 hex characters, got %d", len(trimmed))
	}

	for _, r := range trimmed {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return "", fmt.Errorf("sha256 digest contains a non-hex character %q", r)
		}
	}

	return "sha256-" + strings.ToLower(trimmed), nil
}

func boolPtr(v bool) *bool { return &v }
