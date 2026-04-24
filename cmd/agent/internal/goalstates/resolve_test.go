// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// discardLogger returns a logger that silently drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestResolveOCIImage_ConfigImageTakesPrecedence(t *testing.T) {
	// Even when env vars and GPU are present, configImage wins.
	t.Setenv("AGENT_OCI_IMAGE", "env-image:latest")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := ResolveOCIImage(discardLogger(), "config-image:v1", true)
	assert.Equal(t, "config-image:v1", got)
}

func TestResolveOCIImage_DisableEnvVar(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"true", "true", ""},
		{"1", "1", ""},
		{"TRUE", "TRUE", ""},
		// Falsy or unrecognised values should NOT disable; expect the default image.
		{"false", "false", DefaultOCIImage},
		{"0", "0", DefaultOCIImage},
		{"empty", "", DefaultOCIImage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT_DISABLE_OCI_IMAGE", tt.value)
			t.Setenv("AGENT_OCI_IMAGE", "")

			got := ResolveOCIImage(discardLogger(), "", false)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveOCIImage_DisableDoesNotOverrideConfig(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := ResolveOCIImage(discardLogger(), "config-image:v2", false)
	assert.Equal(t, "config-image:v2", got)
}

func TestResolveOCIImage_EnvVarFallback(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "env-image:v3")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v3", got)
}

func TestResolveOCIImage_EnvVarTrimmed(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "  env-image:v4  ")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v4", got)
}

func TestResolveOCIImage_EnvVarWhitespaceOnly(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "   ")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultNoGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultWithGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := ResolveOCIImage(discardLogger(), "", true)
	assert.Equal(t, DefaultNvidiaOCImage, got)
}

func TestResolveOCIImage_Priority(t *testing.T) {
	// Verify the full priority chain: config > disable > env var > default.
	log := discardLogger()

	// 1. Config set - everything else ignored.
	t.Setenv("AGENT_OCI_IMAGE", "env")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "1")

	assert.Equal(t, "config", ResolveOCIImage(log, "config", true))

	// 2. No config, disable set - returns empty despite env var being set.
	assert.Equal(t, "", ResolveOCIImage(log, "", true))

	// 3. No config, disable off, env var set.
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "0")

	assert.Equal(t, "env", ResolveOCIImage(log, "", true))

	// 4. No config, disable off, no env var - GPU default.
	t.Setenv("AGENT_OCI_IMAGE", "")

	assert.Equal(t, DefaultNvidiaOCImage, ResolveOCIImage(log, "", true))
	assert.Equal(t, DefaultOCIImage, ResolveOCIImage(log, "", false))
}
