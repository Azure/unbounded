// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommandRunner runs external programs that are still delegated to the Ubuntu
// netboot initrd, such as modprobe, efibootmgr, and the emergency shell.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) (string, error)
	LookPath(name string) (string, error)
}

type realCommandRunner struct{}

func (realCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return formatCommandError(name, args, err, out)
	}

	return nil
}

func (realCommandRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", formatCommandError(name, args, err, out)
	}

	return strings.TrimSpace(string(out)), nil
}

func (realCommandRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func formatCommandError(name string, args []string, err error, out []byte) error {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
}
