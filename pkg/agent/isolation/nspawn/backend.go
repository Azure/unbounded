// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// ErrNotWired is returned by lifecycle operations whose implementation has not
// been injected into the Backend (see Ops).
var ErrNotWired = errors.New("nspawn backend operation not configured")

// Ops are the lifecycle operations the backend delegates to. They live in the
// phase packages, which depend on this package, so they are injected by the
// composition root rather than imported. A nil Ops field makes the matching
// Backend method return ErrNotWired.
type Ops struct {
	Prepare func(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error)
	Start   func(ctx context.Context, r isolation.SlotRef) error
	Stop    func(ctx context.Context, r isolation.SlotRef) error
	Restart func(ctx context.Context, r isolation.SlotRef) error
	Destroy func(ctx context.Context, r isolation.SlotRef) error
}

// Backend is the systemd-nspawn isolation backend.
type Backend struct {
	Log *slog.Logger
	Ops Ops
}

var _ isolation.Backend = (*Backend)(nil)

// Mode implements isolation.Backend.
func (*Backend) Mode() isolation.Mode { return isolation.ModeNSpawn }

// Requirements implements isolation.Backend.
func (*Backend) Requirements() isolation.Requirements {
	return isolation.Requirements{
		HostPackages:  []string{"systemd-container"},
		HostCommands:  []string{"systemd-nspawn", "machinectl"},
		WritablePaths: []string{"/etc", "/var"},
	}
}

// Prepare implements isolation.Backend. nspawn derives the machine's devices,
// mounts and libraries from the host when the machine is built, not from
// NodeSpec.Grants, so a request for grants fails rather than being dropped.
func (b *Backend) Prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	if len(n.Grants) > 0 {
		return nil, errors.New("nspawn derives the machine's device, mount and library grants from the host when it is built; NodeSpec.Grants are not honored")
	}

	if b.Ops.Prepare == nil {
		return nil, ErrNotWired
	}

	return b.Ops.Prepare(ctx, n)
}

// Start implements isolation.Backend.
func (b *Backend) Start(ctx context.Context, r isolation.SlotRef) error {
	return callOp(ctx, b.Ops.Start, r)
}

// Stop implements isolation.Backend.
func (b *Backend) Stop(ctx context.Context, r isolation.SlotRef) error {
	return callOp(ctx, b.Ops.Stop, r)
}

// Restart implements isolation.Backend.
func (b *Backend) Restart(ctx context.Context, r isolation.SlotRef) error {
	return callOp(ctx, b.Ops.Restart, r)
}

// Destroy implements isolation.Backend.
func (b *Backend) Destroy(ctx context.Context, r isolation.SlotRef) error {
	return callOp(ctx, b.Ops.Destroy, r)
}

func callOp(ctx context.Context, op func(context.Context, isolation.SlotRef) error, r isolation.SlotRef) error {
	if op == nil {
		return ErrNotWired
	}

	return op(ctx, r)
}

// Status implements isolation.Backend. The machine is running when a command
// can be executed inside it.
func (b *Backend) Status(ctx context.Context, r isolation.SlotRef) (isolation.Status, error) {
	if _, err := Exec(ctx, b.logger(), r.Slot(), "/bin/true"); err != nil {
		// A probe the caller canceled says nothing about the machine.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return isolation.Status{}, ctxErr
		}

		return isolation.Status{Detail: err.Error()}, nil //nolint:nilerr // an unreachable machine is a status, not a failure
	}

	return isolation.Status{Running: true}, nil
}

// Exec implements isolation.Backend.
func (b *Backend) Exec(ctx context.Context, r isolation.SlotRef, cmd isolation.ExecSpec) (isolation.ExecResult, error) {
	out, err := Exec(ctx, b.logger(), r.Slot(), cmd.Args...)

	return isolation.ExecResult{Stdout: out}, err
}

// Logs implements isolation.Backend.
func (*Backend) Logs(ctx context.Context, r isolation.SlotRef, opts isolation.LogOptions) (io.ReadCloser, error) {
	return Logs(ctx, r.Slot(), opts)
}

func (b *Backend) logger() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}

	return b.Log
}
