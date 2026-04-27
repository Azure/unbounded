// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package license

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// FindFile locates the canonical license file inside dir. It searches a
// fixed list of conventional names (LICENSE, LICENCE, COPYING, etc.) and
// returns the first hit. Returns an error if no candidate exists.
func FindFile(dir string) (string, error) {
	candidates := []string{
		"LICENSE", "LICENSE.txt", "LICENSE.md",
		"LICENCE", "LICENCE.txt", "LICENCE.md",
		"COPYING", "COPYING.md",
		"License", "License.txt", "License.md",
	}
	for _, name := range candidates {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("no LICENSE file found in %s", dir)
}

// copyrightRE matches lines that look like real copyright statements. We
// require Copyright to be followed by (c), ©, or a 4-digit year so that
// section headings ("Copyright Licenses:") and references in license body
// text ("Copyright License" clauses) are not picked up as attributions.
var copyrightRE = regexp.MustCompile(`(?m)^\s*Copyright\s+(?:\(c\)|©|\d{4}).*$`)

// ExtractCopyrightFromDir tries to find copyright lines in the LICENSE text
// first, then falls back to a sibling NOTICE / AUTHORS / CONTRIBUTORS file in
// the same directory. If no copyright is found in any of those, returns a
// single-element placeholder pointing the reader at the LICENSE file (common
// for Apache-2.0 projects whose LICENSE is the stock boilerplate and whose
// copyright lives only in source-file headers).
func ExtractCopyrightFromDir(dir string, licenseText []byte) ([]string, error) {
	if cs, err := ExtractCopyright(licenseText); err == nil {
		return cs, nil
	}

	fallbacks := []string{
		"NOTICE", "NOTICE.txt", "NOTICE.md",
		"AUTHORS", "AUTHORS.txt", "AUTHORS.md",
		"CONTRIBUTORS", "CONTRIBUTORS.txt", "CONTRIBUTORS.md",
		"COPYRIGHT", "COPYRIGHT.txt", "COPYRIGHT.md",
	}
	for _, name := range fallbacks {
		p := filepath.Join(dir, name)

		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		if cs, err := ExtractCopyright(data); err == nil {
			return cs, nil
		}
	}

	return []string{"See LICENSE file"}, nil
}

// ExtractCopyright pulls Copyright lines out of license text. Lines that
// look like license-text boilerplate (containing uppercase "COPYRIGHT
// HOLDER(S)" or "COPYRIGHT NOTICE") are ignored. Returns an error when no
// usable copyright line is found.
func ExtractCopyright(text []byte) ([]string, error) {
	matches := copyrightRE.FindAll(text, -1)

	seen := map[string]bool{}

	var out []string

	for _, m := range matches {
		line := strings.TrimSpace(string(m))
		// Filter boilerplate phrases that contain "Copyright" but are not
		// actual copyright statements.
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "COPYRIGHT HOLDER") ||
			strings.Contains(upper, "COPYRIGHT NOTICE") ||
			strings.Contains(upper, "COPYRIGHT OWNER") ||
			strings.Contains(upper, "RETAIN THE ABOVE COPYRIGHT") ||
			strings.Contains(upper, "OF THE COPYRIGHT") {
			continue
		}

		// Trim trailing colons or punctuation that prefixes the next sentence.
		line = strings.TrimRight(line, ":")
		if seen[line] {
			continue
		}

		seen[line] = true

		out = append(out, line)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no copyright line found")
	}

	return out, nil
}
