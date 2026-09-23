// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package racer

import "io"

func (s *Stream) spliceTo(io.Writer) (int64, error, bool) { return 0, nil, false }
