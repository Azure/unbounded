// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/override"
	"github.com/Azure/unbounded/internal/unbounded"
)

func overridesValidateCommand() *cobra.Command {
	var (
		files     []string
		namespace string
	)

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check override documents for syntax and allowlist errors",
		Long: `Check override documents without applying them.

This is offline syntax validation only: parsing rules, schema, apiVersion,
allowlisted and protected paths, strategic merge directives and explicit nulls.
Every one of those is a pure function of the document, so the answer is correct
regardless of which operator version is running.

It deliberately does not check whether a container or volume named by a patch
actually exists. That depends on the workload the running operator renders, and
a plugin built from a different commit would answer from its own copy of the
manifests. Run 'kubectl unbounded overrides status' after applying to confirm
resolution.

Examples:
  kubectl unbounded overrides validate -f overrides.yaml
  kubectl unbounded overrides validate`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(files) > 0 {
				return runOverridesValidateFiles(files, cmd.OutOrStdout())
			}

			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runOverridesValidateCluster(ctx, c, namespace, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringSliceVarP(&files, "filename", "f", nil,
		"Override document to validate. Repeatable. Reads the cluster ConfigMap when omitted.")
	cmd.Flags().StringVar(&namespace, "namespace", unbounded.SystemNamespace(),
		"Namespace holding the overrides ConfigMap")

	return cmd
}

// runOverridesValidateFiles validates local files, which is what makes this
// usable before a document is ever applied.
func runOverridesValidateFiles(files []string, out io.Writer) error {
	data := make(map[string]string, len(files))

	for _, file := range files {
		contents, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}

		data[filepath.Base(file)] = string(contents)
	}

	return reportValidation(data, out)
}

func runOverridesValidateCluster(ctx context.Context, c client.Client, namespace string, out io.Writer) error {
	configMap, found, err := getOverridesConfigMap(ctx, c, namespace)
	if err != nil {
		return err
	}

	if !found {
		fprintln(out, "No overrides ConfigMap found; the operator applies its default manifests.")

		return nil
	}

	return reportValidation(configMap.Data, out)
}

func reportValidation(data map[string]string, out io.Writer) error {
	entries, err := override.Parse(data)
	if err != nil {
		return err
	}

	if err := override.Validate(entries); err != nil {
		return err
	}

	fprintf(out, "ok: %d entr%s, syntax and allowlist valid\n", len(entries), plural(len(entries)))
	fprintln(out, "note: container and volume names are resolved by the operator against the")
	fprintln(out, "      workloads it renders. Run 'kubectl unbounded overrides status' after")
	fprintln(out, "      applying to confirm they resolved.")

	return nil
}

// getOverridesConfigMap reads the overrides ConfigMap, reporting absence
// separately from failure so callers can tell "no overrides" from "cannot
// tell".
func getOverridesConfigMap(ctx context.Context, c client.Client, namespace string) (*corev1.ConfigMap, bool, error) {
	var configMap corev1.ConfigMap

	key := client.ObjectKey{Namespace: namespace, Name: override.ConfigMapName}

	if err := c.Get(ctx, key, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("read overrides ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
	}

	return &configMap, true, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}

	return "ies"
}

func joinNonEmpty(values []string, sep string) string {
	kept := make([]string, 0, len(values))

	for _, value := range values {
		if value != "" {
			kept = append(kept, value)
		}
	}

	return strings.Join(kept, sep)
}
