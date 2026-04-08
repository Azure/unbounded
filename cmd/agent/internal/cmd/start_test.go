package cmd

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
)

// discardLogger returns a logger that silently drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestResolveOCIImage_ConfigImageTakesPrecedence(t *testing.T) {
	// Even when env vars and GPU are present, configImage wins.
	t.Setenv("AGENT_OCI_IMAGE", "env-image:latest")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := resolveOCIImage(discardLogger(), "config-image:v1", true)
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
		{"false", "false", goalstates.DefaultOCIImage},
		{"0", "0", goalstates.DefaultOCIImage},
		{"empty", "", goalstates.DefaultOCIImage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT_DISABLE_OCI_IMAGE", tt.value)
			t.Setenv("AGENT_OCI_IMAGE", "")

			got := resolveOCIImage(discardLogger(), "", false)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveOCIImage_DisableDoesNotOverrideConfig(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := resolveOCIImage(discardLogger(), "config-image:v2", false)
	assert.Equal(t, "config-image:v2", got)
}

func TestResolveOCIImage_EnvVarFallback(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "env-image:v3")

	got := resolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v3", got)
}

func TestResolveOCIImage_EnvVarTrimmed(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "  env-image:v4  ")

	got := resolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v4", got)
}

func TestResolveOCIImage_EnvVarWhitespaceOnly(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "   ")

	got := resolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, goalstates.DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultNoGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := resolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, goalstates.DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultWithGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := resolveOCIImage(discardLogger(), "", true)
	assert.Equal(t, goalstates.DefaultNvidiaOCImage, got)
}

func TestResolveOCIImage_Priority(t *testing.T) {
	// Verify the full priority chain: config > disable > env var > default.
	log := discardLogger()

	// 1. Config set — everything else ignored.
	t.Setenv("AGENT_OCI_IMAGE", "env")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "1")

	assert.Equal(t, "config", resolveOCIImage(log, "config", true))

	// 2. No config, disable set — returns empty despite env var being set.
	assert.Equal(t, "", resolveOCIImage(log, "", true))

	// 3. No config, disable off, env var set.
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "0")

	assert.Equal(t, "env", resolveOCIImage(log, "", true))

	// 4. No config, disable off, no env var — GPU default.
	t.Setenv("AGENT_OCI_IMAGE", "")

	assert.Equal(t, goalstates.DefaultNvidiaOCImage, resolveOCIImage(log, "", true))
	assert.Equal(t, goalstates.DefaultOCIImage, resolveOCIImage(log, "", false))
}
