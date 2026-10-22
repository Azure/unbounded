// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package racersdk

import "errors"

// mappedSupported reports whether this platform requests mapped delivery.
// Other platforms always ask for inline delivery.
const mappedSupported = false

type rightsReader struct{}

func (*rightsReader) take() ([]int, error) { return nil, nil }

func receiveRights(*clientConn) {}

func closeFDs([]int) {}

type mapping struct{ data []byte }

type mappings struct{}

func (*mappings) acquire(int) (*mapping, error) {
	return nil, errors.New("mapped delivery is not supported")
}

func (*mappings) release(*mapping) {}

func (*mappings) close() {}
