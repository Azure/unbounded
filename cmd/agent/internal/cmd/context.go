// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"log/slog"
	"os"

	"github.com/Azure/unbounded/internal/logger"
)

// CommandContext holds cross-cutting configuration shared by all agent
// subcommands: logging.
//
// Persistent flags are bound to LogOpts in Run(), and Setup() is called inside
// each subcommand's RunE, after cobra has finished parsing, to initialize
// Logger.
type CommandContext struct {
	LogOpts logger.Options
	Logger  *slog.Logger
}

// newCommandContext returns the agent's logging defaults: prettytext with the
// task attribute first, which the bootstrap log tooling parses.
func newCommandContext() *CommandContext {
	return &CommandContext{LogOpts: logger.Options{Format: logger.FormatPrettyText, AttrOrder: []string{"task"}}}
}

// Setup installs the logger for the current flag values and sets Logger.
func (c *CommandContext) Setup() {
	c.Logger = c.LogOpts.Setup(os.Stderr)
}
