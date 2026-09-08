// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckLocalDNSConntrackDisabled(t *testing.T) {
	deps := localDNSConntrackDeps{
		lookupPath: func(string) (string, error) {
			t.Fatal("lookupPath() must not be called")

			return "", nil
		},
	}

	results := checkLocalDNSConntrack(slog.New(slog.DiscardHandler), config.AgentConfig{}, deps).Check(t.Context())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.Contains(t, results[0].Message, "disabled")
}

func TestCheckLocalDNSConntrackUsesNFTables(t *testing.T) {
	var commands []string

	deps := localDNSConntrackDeps{
		lookupPath: lookupPathWith(map[string]bool{"nft": true}),
		run: func(_ context.Context, name string, _ []string, stdin string) ([]byte, error) {
			commands = append(commands, name)

			assert.Contains(t, stdin, "priority raw")
			assert.Contains(t, stdin, "notrack")

			return nil, nil
		},
	}
	cfg := config.AgentConfig{LocalDNS: &config.AgentLocalDNSConfig{Enabled: true}}

	results := checkLocalDNSConntrack(slog.New(slog.DiscardHandler), cfg, deps).Check(t.Context())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.Contains(t, results[0].Message, "nftables")
	assert.Equal(t, []string{"nft"}, commands)
}

func TestCheckLocalDNSConntrackRejectsMissingNFTablesOffline(t *testing.T) {
	deps := localDNSConntrackDeps{
		lookupPath: lookupPathWith(nil),
		run: func(context.Context, string, []string, string) ([]byte, error) {
			t.Fatal("run() must not be called")

			return nil, nil
		},
	}
	cfg := config.AgentConfig{
		LocalDNS:         &config.AgentLocalDNSConfig{Enabled: true},
		OfflineArtifacts: &config.AgentOfflineArtifacts{Source: "file:///bundle"},
	}

	results := checkLocalDNSConntrack(slog.New(slog.DiscardHandler), cfg, deps).Check(t.Context())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
}

func TestCheckLocalDNSConntrackRejectsUnsupportedNFTables(t *testing.T) {
	deps := localDNSConntrackDeps{
		lookupPath: lookupPathWith(map[string]bool{"nft": true}),
		run: func(context.Context, string, []string, string) ([]byte, error) {
			return nil, errors.New("notrack unsupported")
		},
	}
	cfg := config.AgentConfig{LocalDNS: &config.AgentLocalDNSConfig{Enabled: true}}

	results := checkLocalDNSConntrack(slog.New(slog.DiscardHandler), cfg, deps).Check(t.Context())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "notrack")
}
