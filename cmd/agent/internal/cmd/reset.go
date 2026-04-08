package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"github.com/project-unbounded/unbounded-kube/internal/provision"
	"github.com/project-unbounded/unbounded-kube/internal/version"
)

// defaultConfigPath is the well-known location for the agent config file
// written by cloud-init based bootstrapping.
const defaultConfigPath = "/etc/unbounded-agent/config.json"

func newCmdReset(cmdCtx *CommandContext) *cobra.Command {
	var machineName string

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset the host by removing the agent and all associated resources",
		Long: `Fully reverse the bootstrap process: stop and remove the nspawn machine,
clean up network interfaces, remove configuration files, and restore the host
to its original state. This is the inverse of 'unbounded-agent start'.

The machine name is resolved in this order:
  1. --machine-name flag
  2. Agent config file (UNBOUNDED_AGENT_CONFIG_FILE env var)
  3. Default config path (/etc/unbounded-agent/config.json)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			cmdCtx.Logger.Info("starting unbounded-agent reset",
				"version", version.Version,
				"commit", version.GitCommit,
			)

			name, err := resolveMachineName(machineName)
			if err != nil {
				return err
			}

			cmdCtx.Logger.Info("resetting host", "machine", name)

			script := provision.UnboundedAgentUninstallScript(name)

			return runResetScript(ctx, script)
		},
	}

	cmd.Flags().StringVar(&machineName, "machine-name", "", "Name of the machine to reset (overrides config file)")

	return cmd
}

// resolveMachineName determines the machine name from the flag, the agent
// config file, or a well-known default path.
func resolveMachineName(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	// Try config file from environment variable.
	if path := strings.TrimSpace(os.Getenv(configFileEnv)); path != "" {
		cfg, err := loadConfigFromFile(path)
		if err != nil {
			return "", fmt.Errorf("reading config for machine name: %w", err)
		}

		if cfg.MachineName != "" {
			return cfg.MachineName, nil
		}
	}

	// Try well-known config path.
	if cfg, err := loadConfigFromFile(defaultConfigPath); err == nil && cfg.MachineName != "" {
		return cfg.MachineName, nil
	}

	return "", fmt.Errorf(
		"machine name is required: use --machine-name flag or ensure agent config is available via %s or %s",
		configFileEnv, defaultConfigPath,
	)
}

// runResetScript executes the rendered uninstall script via bash.
func runResetScript(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "bash", "-e", "-o", "pipefail")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reset script failed: %w", err)
	}

	return nil
}
