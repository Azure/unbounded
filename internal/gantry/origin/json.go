// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package origin

import (
	"encoding/json"
	"io"
)

// decodeJSON is a tiny helper so the main file doesn't import encoding/json
// at the top level; keeps the dependency footprint visible at a glance.
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
