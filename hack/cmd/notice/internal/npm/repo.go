// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package npm

import (
	"encoding/json"
	"strings"
)

// packageJSON is a minimal subset of the npm package manifest schema.
type packageJSON struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	License    json.RawMessage   `json:"license"`
	Licenses   []legacyLicense   `json:"licenses"`
	Repository json.RawMessage   `json:"repository"`
	Funding    json.RawMessage   `json:"funding"`
	Deps       map[string]string `json:"dependencies"`
	DevDeps    map[string]string `json:"devDependencies"`
	Engines    map[string]string `json:"engines"`
	Bugs       json.RawMessage   `json:"bugs"`
	Author     json.RawMessage   `json:"author"`
	Extras     map[string]any    `json:"-"`
}

type legacyLicense struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// licenseString returns the SPDX-shaped license identifier declared in
// package.json, accommodating modern, legacy object, and plural-array forms.
func licenseString(pkg packageJSON) string {
	// Modern: "license": "MIT" (string).
	var s string
	if err := json.Unmarshal(pkg.License, &s); err == nil && s != "" {
		return s
	}

	// Legacy: "license": {"type": "MIT", "url": "..."}.
	var obj struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(pkg.License, &obj); err == nil && obj.Type != "" {
		return obj.Type
	}

	// Even older: "licenses": [{"type": "MIT", ...}].
	if len(pkg.Licenses) > 0 {
		return pkg.Licenses[0].Type
	}

	return ""
}

// repoBase derives a repository base URL from a package's `repository`
// field. The field may be a string or an object; URLs may be prefixed with
// "git+" or suffixed with ".git". Falls back to the npm package URL when the
// field is missing.
func repoBase(pkg packageJSON, name string) (base, ref string) {
	ref = "HEAD"

	var raw string

	var asString string
	if err := json.Unmarshal(pkg.Repository, &asString); err == nil && asString != "" {
		raw = asString
	} else {
		var obj struct {
			Type      string `json:"type"`
			URL       string `json:"url"`
			Directory string `json:"directory"`
		}
		if err := json.Unmarshal(pkg.Repository, &obj); err == nil {
			raw = obj.URL
		}
	}

	if raw == "" {
		return "https://www.npmjs.com/package/" + name, ref
	}

	raw = strings.TrimPrefix(raw, "git+")
	raw = strings.TrimSuffix(raw, ".git")
	raw = strings.TrimPrefix(raw, "git://")

	if strings.HasPrefix(raw, "ssh://git@") {
		raw = "https://" + strings.TrimPrefix(raw, "ssh://git@")
	}

	if !strings.HasPrefix(raw, "http") {
		raw = "https://" + raw
	}

	// Strip a trailing "/tree/<ref>/..." or "/blob/<ref>/..." segment if the
	// repository field embeds one (e.g. monorepo packages).
	if i := strings.Index(raw, "/tree/"); i > 0 {
		raw = raw[:i]
	}

	if i := strings.Index(raw, "/blob/"); i > 0 {
		raw = raw[:i]
	}

	return raw, ref
}
