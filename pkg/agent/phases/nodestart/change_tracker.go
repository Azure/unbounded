// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"os"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// changeTracker records whether any file a configuration task owns actually
// differed from what was already on disk.
//
// The node services read their configuration at start, so a reapply that alters
// one has to restart it and a reapply that alters nothing must not. Embedders
// write through this rather than calling utilio directly, so the answer covers
// every file the task owns rather than only the last one written.
type changeTracker struct {
	changed bool
}

// write applies content and records whether it differed from what was there.
func (t *changeTracker) write(path string, content []byte, perm os.FileMode) error {
	changed, err := utilio.WriteFileIfChanged(path, content, perm)
	if err != nil {
		return err
	}

	t.changed = t.changed || changed

	return nil
}
