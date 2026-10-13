// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded/internal/logger"
)

type CommandContext struct {
	AzureCli       azsdk.ClientSet
	CloudName      string
	DataDir        string
	SubscriptionID string
	Location       string
	LogOpts        logger.Options
	Logger         *slog.Logger
}

func (cfg *CommandContext) Setup() error {
	cfg.AzureCli = azsdk.ClientSet{
		CloudName:      cfg.CloudName,
		SubscriptionID: cfg.SubscriptionID,
	}

	cfg.Logger = cfg.LogOpts.Setup(os.Stderr)

	if err := cfg.AzureCli.Configure(); err != nil {
		return fmt.Errorf("configure azure clients: %w", err)
	}

	return nil
}
