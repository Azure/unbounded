// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisableSystemdUnits(t *testing.T) {
	var commands [][]string

	task := &disableSystemdUnits{
		name:  "disable-test",
		units: []string{"first.service", "second.service"},
		log:   slog.New(slog.DiscardHandler),
		runCmdAt: func(_ context.Context, _ *slog.Logger, _ slog.Level, _ func(context.Context) *exec.Cmd, args ...string) error {
			commands = append(commands, args)

			return nil
		},
	}

	require.NoError(t, task.ensureDisabled(context.Background()))
	assert.Equal(t, [][]string{
		{"stop", "first.service"},
		{"mask", "first.service"},
		{"stop", "second.service"},
		{"mask", "second.service"},
	}, commands)
}

func TestDisableSystemdUnitsIgnoresStopErrors(t *testing.T) {
	task := &disableSystemdUnits{
		name:  "disable-test",
		units: []string{"test.service"},
		log:   slog.New(slog.DiscardHandler),
		runCmdAt: func(_ context.Context, _ *slog.Logger, _ slog.Level, _ func(context.Context) *exec.Cmd, args ...string) error {
			if args[0] == "stop" {
				return errors.New("unit missing")
			}

			return nil
		},
	}

	require.NoError(t, task.ensureDisabled(context.Background()))
}

func TestDisableSystemdUnitsReturnsMaskError(t *testing.T) {
	task := &disableSystemdUnits{
		name:  "disable-test",
		units: []string{containerdServiceUnit},
		log:   slog.New(slog.DiscardHandler),
		runCmdAt: func(_ context.Context, _ *slog.Logger, _ slog.Level, _ func(context.Context) *exec.Cmd, args ...string) error {
			if args[0] == "mask" && args[1] == containerdServiceUnit {
				return errors.New("mask failed")
			}

			return nil
		},
	}

	err := task.ensureDisabled(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), containerdServiceUnit)
}

func TestDisableSystemdUnitsPreservesLocalUnit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, kubeletServiceUnit)
	require.NoError(t, os.WriteFile(path, []byte("unit"), 0o644))

	var commands []string
	task := &disableSystemdUnits{
		name: "disable-test", units: []string{kubeletServiceUnit}, unitDir: dir,
		log: slog.New(slog.DiscardHandler),
		runCmdAt: func(_ context.Context, _ *slog.Logger, _ slog.Level, _ func(context.Context) *exec.Cmd, args ...string) error {
			commands = append(commands, args[0])
			if args[0] == "mask" {
				return errors.New("file exists")
			}
			return nil
		},
	}

	require.NoError(t, task.Do(t.Context()))
	require.Equal(t, []string{"stop", "mask", "disable"}, commands)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "unit", string(data))

	task.runCmdAt = func(_ context.Context, _ *slog.Logger, _ slog.Level, _ func(context.Context) *exec.Cmd, args ...string) error {
		if args[0] != "stop" {
			return errors.New("failed")
		}
		return nil
	}
	require.ErrorContains(t, task.Do(t.Context()), "disabling kubelet.service")

	task.runCmdAt = func(_ context.Context, _ *slog.Logger, _ slog.Level, _ func(context.Context) *exec.Cmd, args ...string) error {
		return errors.New("failed " + args[0])
	}
	require.ErrorContains(t, task.Do(t.Context()), "stopping kubelet.service")
}

func TestDisableServiceTasks(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	assert.Equal(t, "disable-docker", DisableDocker(log).Name())
	assert.Equal(t, "disable-containerd", DisableContainerd(log).Name())
	assert.Equal(t, "disable-kubelet", DisableKubelet(log).Name())
}
