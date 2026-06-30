// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/agent-artifacts-builder/artifacts"
	"github.com/Azure/unbounded/internal/logger"
)

const (
	defaultKubernetesVersionsFile = "hack/cmd/agent-artifacts-builder/kubernetes-versions.txt"
	artifactTagRefPrefix          = "agent-artifacts/"
)

//go:embed kubernetes-versions.txt
var defaultKubernetesVersionsFS embed.FS

func main() {
	if err := newRootCommand().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var (
		opts      artifacts.Options
		debug     bool
		logFormat string
	)

	cmd := &cobra.Command{
		Use:          "agent-artifacts-builder",
		Short:        "Build offline agent bootstrap artifact bundles",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := newLogger(debug, logFormat)

			if opts.ManifestPath == "" && opts.KubernetesVersion == "" {
				return fmt.Errorf("one of --manifest or --kubernetes-version is required")
			}

			if opts.ManifestPath != "" && opts.KubernetesVersion != "" {
				return fmt.Errorf("--manifest and --kubernetes-version are mutually exclusive")
			}

			return artifacts.Build(cmd.Context(), log, opts)
		},
	}

	cmd.PersistentFlags().BoolVar(&debug, "debug", false, "enable debug-level logging")
	cmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "log format: text or json")

	flags := cmd.Flags()
	flags.StringVar(&opts.OutputDir, "output-dir", "", "Directory where the offline artifact filesystem layout is written")
	flags.StringVar(&opts.OCIRef, "oci-ref", "", "Optional OCI artifact reference to push, with or without oci:// prefix")
	flags.StringVar(&opts.ManifestPath, "manifest", "", "Path to offline artifact manifest.json declaring artifact versions")
	flags.StringVar(&opts.KubernetesVersion, "kubernetes-version", "", "Kubernetes version for a default manifest using the agent's pinned runtime versions")
	flags.StringSliceVar(&opts.Architectures, "arch", nil, "Target architecture to include. Repeat or comma separate. Defaults to the host GOARCH")
	flags.BoolVar(&opts.SkipExisting, "skip-existing", false, "Reuse existing files in output dir instead of downloading them again")

	cmd.AddCommand(newResolvePublishInputsCommand())

	return cmd
}

func newLogger(debug bool, format string) *slog.Logger {
	var lvl slog.LevelVar
	if debug {
		lvl.Set(slog.LevelDebug)
	}

	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &lvl}))
	}

	return slog.New(logger.NewPrettyFieldHandler(&lvl, logger.PrettyFieldHandlerOptions{
		AttrOrder: []string{"artifact", "source", "oci_ref", "digest"},
	}))
}

func newResolvePublishInputsCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "resolve-publish-inputs",
		Short:        "Resolve GitHub Actions inputs for publishing offline artifact bundles",
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return resolvePublishInputs()
		},
	}
}

func resolvePublishInputs() error {
	eventName := os.Getenv("EVENT_NAME")
	refName := os.Getenv("REF_NAME")
	inputTag := strings.TrimSpace(os.Getenv("INPUT_TAG"))
	inputVersions := strings.TrimSpace(os.Getenv("INPUT_KUBERNETES_VERSIONS"))
	githubSHA := os.Getenv("GITHUB_SHA_VALUE")

	var (
		tag         string
		versionsRaw string
	)
	if eventName == "push" {
		tag = strings.TrimPrefix(refName, artifactTagRefPrefix)

		var err error

		versionsRaw, err = defaultKubernetesVersions()
		if err != nil {
			return err
		}
	} else {
		tag = inputTag
		if tag == "" {
			tag = shortSHA(githubSHA)
		}

		versionsRaw = inputVersions
		if versionsRaw == "" {
			var err error

			versionsRaw, err = defaultKubernetesVersions()
			if err != nil {
				return err
			}
		}
	}

	if tag == "" {
		return fmt.Errorf("artifact tag could not be resolved")
	}

	versions := normalizeKubernetesVersions(versionsRaw)
	if len(versions) == 0 {
		return fmt.Errorf("at least one Kubernetes version is required")
	}

	versionsJSON, err := json.Marshal(versions)
	if err != nil {
		return fmt.Errorf("marshal Kubernetes versions: %w", err)
	}

	if err := writeGitHubOutput(map[string]string{
		"tag":                 tag,
		"kubernetes_versions": string(versionsJSON),
	}); err != nil {
		return err
	}

	fmt.Printf("Publishing tag prefix: %s\n", tag)
	fmt.Printf("Kubernetes versions: %s\n", versionsJSON)

	return nil
}

func defaultKubernetesVersions() (string, error) {
	path := strings.TrimSpace(os.Getenv("DEFAULT_KUBERNETES_VERSIONS_FILE"))
	if path != "" {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return "", fmt.Errorf("read default Kubernetes versions file %q: %w", path, err)
		}

		return stripLineComments(string(data)), nil
	}

	data, err := defaultKubernetesVersionsFS.ReadFile("kubernetes-versions.txt")
	if err != nil {
		return "", fmt.Errorf("read embedded default Kubernetes versions: %w", err)
	}

	return stripLineComments(string(data)), nil
}

func stripLineComments(raw string) string {
	var out []string

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		out = append(out, line)
	}

	return strings.Join(out, "\n")
}

func normalizeKubernetesVersions(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})

	versions := make([]string, 0, len(fields))
	for _, field := range fields {
		version := strings.TrimSpace(field)
		if version == "" {
			continue
		}

		if !strings.HasPrefix(version, "v") {
			version = "v" + version
		}

		versions = append(versions, version)
	}

	return versions
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}

	return sha[:12]
}

func writeGitHubOutput(values map[string]string) error {
	outputPath := os.Getenv("GITHUB_OUTPUT")
	if outputPath == "" {
		for key, value := range values {
			fmt.Printf("%s=%s\n", key, value)
		}

		return nil
	}

	file, err := os.OpenFile(outputPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open GITHUB_OUTPUT: %w", err)
	}
	defer file.Close() //nolint:errcheck // best effort close

	for key, value := range values {
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			return fmt.Errorf("write GITHUB_OUTPUT: %w", err)
		}
	}

	return nil
}
