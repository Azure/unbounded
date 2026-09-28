// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package racersdk

import "context"

func spliceBody(context.Context, *responseBody, *FDSink, int64) (int64, bool, error) {
	return 0, false, nil
}
