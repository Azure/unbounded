// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"context"
	"io"
	"strings"
	"sync"
)

// FakeBackend records every call and returns configured results. It is safe for
// concurrent use. Set the Err fields to inject failures.
type FakeBackend struct {
	FakeMode Mode
	// Dir is the directory FakeRoot paths resolve under.
	Dir string

	PrepareErr, StartErr, StopErr, RestartErr, DestroyErr, StatusErr, ExecErr error

	// ExecFunc, when set, produces the result of Exec.
	ExecFunc func(ExecSpec) (ExecResult, error)

	mu    sync.Mutex
	calls []string
	execs []ExecSpec
}

// Calls returns the recorded operations in order, as "op slot" strings.
func (f *FakeBackend) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

// Execs returns the recorded Exec commands in order.
func (f *FakeBackend) Execs() []ExecSpec {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]ExecSpec(nil), f.execs...)
}

func (f *FakeBackend) record(op, slot string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, op+" "+slot)
}

// Mode implements Backend.
func (f *FakeBackend) Mode() Mode {
	if f.FakeMode == "" {
		return DefaultMode
	}

	return f.FakeMode
}

// Requirements implements Backend.
func (f *FakeBackend) Requirements() Requirements { return Requirements{} }

// Prepare implements Backend.
func (f *FakeBackend) Prepare(_ context.Context, n NodeSpec) (NodeRoot, error) {
	f.record("prepare", n.Slot)

	if f.PrepareErr != nil {
		return nil, f.PrepareErr
	}

	return DirRoot{SlotName: n.Slot, Dir: f.Dir}, nil
}

// Start implements Backend.
func (f *FakeBackend) Start(_ context.Context, r NodeRoot) error {
	f.record("start", r.Slot())
	return f.StartErr
}

// Stop implements Backend.
func (f *FakeBackend) Stop(_ context.Context, r NodeRoot) error {
	f.record("stop", r.Slot())
	return f.StopErr
}

// Restart implements Backend.
func (f *FakeBackend) Restart(_ context.Context, r NodeRoot) error {
	f.record("restart", r.Slot())
	return f.RestartErr
}

// Destroy implements Backend.
func (f *FakeBackend) Destroy(_ context.Context, r NodeRoot) error {
	f.record("destroy", r.Slot())
	return f.DestroyErr
}

// Status implements Backend.
func (f *FakeBackend) Status(_ context.Context, r NodeRoot) (Status, error) {
	f.record("status", r.Slot())

	if f.StatusErr != nil {
		return Status{}, f.StatusErr
	}

	return Status{}, nil
}

// Exec implements Backend.
func (f *FakeBackend) Exec(_ context.Context, r NodeRoot, cmd ExecSpec) (ExecResult, error) {
	f.record("exec", r.Slot())

	f.mu.Lock()
	f.execs = append(f.execs, cmd)
	f.mu.Unlock()

	if f.ExecFunc != nil {
		return f.ExecFunc(cmd)
	}

	return ExecResult{}, f.ExecErr
}

// Logs implements Backend.
func (f *FakeBackend) Logs(_ context.Context, r NodeRoot, _ LogOptions) (io.ReadCloser, error) {
	f.record("logs", r.Slot())
	return io.NopCloser(strings.NewReader("")), nil
}

var _ Backend = (*FakeBackend)(nil)
