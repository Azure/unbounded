// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package clean

import "errors"

// SystemDiscarder is unavailable off Linux, where there is no ublk device to
// trim in the first place.
type SystemDiscarder struct{}

// Discard always fails.
func (SystemDiscarder) Discard(string, uint64, uint64) error {
	return errors.New("clean: discard is only implemented on linux")
}
