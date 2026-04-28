// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/unbounded/internal/logger"
)

// CommandContext holds cross-cutting configuration shared by all agent
// subcommands.  It mirrors the pattern used in forge (see
// hack/cmd/forge/forge/cmd/common.go) but carries only the fields relevant to
// the agent: logging configuration.
//
// Persistent flags are bound directly to this struct's exported fields in
// Run(), and Setup() is called inside each subcommand's RunE — after cobra has
// finished parsing — to initialise the Logger.
type CommandContext struct {
	Debug      bool
	LogFormat  string
	LogNoColor bool
	Logger     *slog.Logger
}

// Setup initialises the Logger from the current flag values.
func (c *CommandContext) Setup() {
	var lvl slog.LevelVar
	if c.Debug {
		lvl.Set(slog.LevelDebug)
	}

	if strings.EqualFold(c.LogFormat, "json") {
		c.Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: &lvl,
		}))
	} else {
		c.Logger = slog.New(logger.NewPrettyFieldHandler(&lvl, logger.PrettyFieldHandlerOptions{
			AttrOrder: []string{"task"},
		}))
	}
}
