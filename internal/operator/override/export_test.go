// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

// parseAll is Parse folded back into a single error.
//
// Parse reports per-key and per-entry failures separately so the operator can
// withhold only the workloads a failure puts in doubt. Most tests here predate
// that and only care whether a document is acceptable at all, so this keeps
// them saying what they mean. Tests that are about attribution use Parse
// directly and inspect the problems.
func parseAll(data map[string]string) ([]SourcedEntry, error) {
	entries, problems, err := Parse(data)
	if err != nil {
		return nil, err
	}

	return entries, ProblemsError(problems)
}
