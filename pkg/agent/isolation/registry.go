// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"fmt"
	"sync"
)

// Factory builds a Backend. deps is backend specific and may be nil.
type Factory func(deps any) (Backend, error)

var (
	registryMu sync.RWMutex
	registry   = map[Mode]Factory{}
)

// Register makes a backend available to New. It is meant to be called from the
// backend package's init or from the composition root. Registering a mode twice
// panics, and so does a nil factory, since both are programming errors that
// would otherwise show up far from the call that is wrong.
func Register(mode Mode, f Factory) {
	if f == nil {
		panic(fmt.Sprintf("isolation: backend %q registered with a nil factory", mode))
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[mode]; dup {
		panic(fmt.Sprintf("isolation: backend %q registered twice", mode))
	}

	registry[mode] = f
}

// factory returns the registered factory for mode. The empty mode is the
// default mode. An unknown or unregistered mode is an error.
func factory(mode Mode) (Mode, Factory, error) {
	m, err := ParseMode(string(mode))
	if err != nil {
		return "", nil, err
	}

	registryMu.RLock()

	f, ok := registry[m]

	registryMu.RUnlock()

	if !ok {
		return "", nil, fmt.Errorf("isolation mode %q has no registered backend", m)
	}

	return m, f, nil
}

// New returns the backend for mode. The empty mode is the default mode. An
// unknown or unregistered mode is an error.
func New(mode Mode, deps any) (Backend, error) {
	_, f, err := factory(mode)
	if err != nil {
		return nil, err
	}

	b, err := f(deps)
	if err != nil {
		return nil, err
	}

	if b == nil {
		return nil, fmt.Errorf("isolation mode %q: the registered factory built no backend", mode)
	}

	return b, nil
}

// Registered returns nil when mode is a known mode with a registered backend,
// and the error New would return otherwise. It builds nothing, so callers that
// only need to know whether this agent can operate in a mode use it instead of
// constructing a backend and discarding it.
func Registered(mode Mode) error {
	_, _, err := factory(mode)

	return err
}
