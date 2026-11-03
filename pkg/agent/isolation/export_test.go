// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

// Unregister removes the backend for mode and returns its factory, so a test
// can exercise the "known mode with no registered backend" path whichever modes
// this binary ships, and then put the factory back with Register.
func Unregister(mode Mode) (Factory, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()

	f, ok := registry[mode]
	delete(registry, mode)

	return f, ok
}
