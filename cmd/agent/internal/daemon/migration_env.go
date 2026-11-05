// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import "path/filepath"

// InPlaceJournalName is the file name of the in-place migration journal, which
// lives next to the applied configs. While a migration has two applied configs
// recorded, it says which one the node runs on.
const InPlaceJournalName = "isolation-migration.json"

// inPlaceJournalPathIn is the in-place journal in the agent configuration
// directory dir.
func inPlaceJournalPathIn(dir string) string { return filepath.Join(dir, InPlaceJournalName) }
