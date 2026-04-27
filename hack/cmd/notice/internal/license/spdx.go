// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package license

import "strings"

var spdxFriendly = map[string]string{
	"MIT":           "MIT License",
	"Apache-2.0":    "Apache License, Version 2.0",
	"BSD-2-Clause":  "BSD 2-Clause License",
	"BSD-3-Clause":  "BSD 3-Clause License",
	"ISC":           "ISC License",
	"MPL-2.0":       "Mozilla Public License, Version 2.0",
	"BlueOak-1.0.0": "Blue Oak Model License 1.0.0",
	"CC0-1.0":       "Creative Commons Zero v1.0 Universal",
	"CC-BY-3.0":     "Creative Commons Attribution 3.0",
	"CC-BY-4.0":     "Creative Commons Attribution 4.0",
	"Unlicense":     "The Unlicense",
	"WTFPL":         "Do What The F*ck You Want To Public License",
	"0BSD":          "BSD Zero Clause License",
	"GPL-2.0":       "GNU General Public License, Version 2.0",
	"GPL-3.0":       "GNU General Public License, Version 3.0",
	"LGPL-2.1":      "GNU Lesser General Public License, Version 2.1",
	"LGPL-3.0":      "GNU Lesser General Public License, Version 3.0",
	"MIT-0":         "MIT No Attribution License",
	"Zlib":          "zlib License",
	"OpenSSL":       "OpenSSL License",
	"BSL-1.0":       "Boost Software License 1.0",
	"PostgreSQL":    "PostgreSQL License",
	"Python-2.0":    "Python License, Version 2.0",
	"Ruby":          "Ruby License",
}

// SPDXFriendly converts an SPDX license identifier to its long human-readable
// name. Unknown identifiers and compound expressions ("MIT OR Apache-2.0")
// are reduced by taking the first known sub-expression; if nothing matches
// the input is returned unchanged.
func SPDXFriendly(spdx string) string {
	if v, ok := spdxFriendly[spdx]; ok {
		return v
	}

	// Compound SPDX expressions (e.g. "MIT OR Apache-2.0") - take the first
	// piece for the friendly mapping; the classifier output covers the rest.
	if i := strings.IndexAny(spdx, " ("); i > 0 {
		if v, ok := spdxFriendly[strings.TrimSpace(spdx[:i])]; ok {
			return v
		}
	}

	return spdx
}
