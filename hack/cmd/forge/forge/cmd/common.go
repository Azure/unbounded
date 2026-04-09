// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded-kube/internal/logger"
)

type CommandContext struct {
	AzureCli       azsdk.ClientSet
	CloudName      string
	DataDir        string
	SubscriptionID string
	Location       string
	LogFormat      string
	LogNoColor     bool
	Logger         *slog.Logger
}

func (cfg *CommandContext) Setup() error {
	cfg.AzureCli = azsdk.ClientSet{
		CloudName:      cfg.CloudName,
		SubscriptionID: cfg.SubscriptionID,
	}

	logLevel := slog.LevelDebug

	cfg.Logger = slog.New(logger.NewPrettyFieldHandler(nil, logger.PrettyFieldHandlerOptions{
		Level:     logLevel,
		AttrOrder: []string{"cluster", "stage", "step"},
	}))

	if strings.EqualFold(cfg.LogFormat, "json") {
		cfg.Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: logLevel,
		}))
	}

	if err := cfg.AzureCli.Configure(); err != nil {
		return fmt.Errorf("configure azure clients: %w", err)
	}

	return nil
}
