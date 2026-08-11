// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

A file may be either a bare overrides document or the ConfigMap manifest you
would apply. A ConfigMap is unwrapped and each of its data keys checked
separately, which is how the operator reads it.

Examples:
  kubectl unbounded overrides validate -f overrides.yaml
  kubectl unbounded overrides validate -f component-overrides.yaml
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
	data, err := loadOverrideFiles(files)
	if err != nil {
		return err
	}

	return reportValidation(data, out)
}

// loadOverrideFiles turns the named files into the ConfigMap-shaped payload the
// operator parses, so validation runs on exactly the same input either way.
//
// A file is accepted in both shapes users actually have. A bare overrides
// document is keyed by its base name, which is what it would become as a
// ConfigMap key. A ConfigMap manifest is unwrapped and each of its data keys
// validated separately, because that is how the operator reads it: the shipped
// example is a ConfigMap manifest, and its own comments tell the reader to
// validate it with this command, which used to fail with three complaints about
// fields missing from an internal Go type.
//
// Keys must be unique across every input. Two files with the same base name
// cannot both become keys of one ConfigMap, and silently keeping the last one
// reported a clean bill of health for a document that was never read.
func loadOverrideFiles(files []string) (map[string]string, error) {
	data := make(map[string]string, len(files))
	origin := make(map[string]string, len(files))

	add := func(key, contents, from string) error {
		if previous, clash := origin[key]; clash {
			return fmt.Errorf("%s and %s both supply the key %q; "+
				"a ConfigMap holds one value per key, so rename one of them", previous, from, key)
		}

		origin[key] = from
		data[key] = contents

		return nil
	}

	for _, file := range files {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}

		wrapped, isConfigMap, err := unwrapConfigMap(contents)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}

		if !isConfigMap {
			if err := add(filepath.Base(file), string(contents), file); err != nil {
				return nil, err
			}

			continue
		}

		for _, key := range sortedKeys(wrapped) {
			if err := add(key, wrapped[key], file+" (key "+key+")"); err != nil {
				return nil, err
			}
		}
	}

	return data, nil
}

// unwrapConfigMap returns the data of a ConfigMap manifest, reporting whether
// the input was one at all.
//
// Every YAML document in the file must agree: a file is either a ConfigMap
// manifest or a bare overrides document. A mixture is refused rather than
// half-read, because silently skipping the part that did not fit is how a
// document goes unvalidated while the command prints ok.
func unwrapConfigMap(contents []byte) (map[string]string, bool, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)

	var (
		data      map[string]string
		found     int
		documents int
	)

	for {
		var configMap corev1.ConfigMap

		if err := decoder.Decode(&configMap); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			// The document is not a ConfigMap. That is the bare-document case,
			// which the override parser reports on properly.
			return nil, false, nil
		}

		if configMap.Kind == "" && configMap.APIVersion == "" && configMap.Data == nil {
			// An empty document, produced by a leading --- or a comment block.
			continue
		}

		documents++

		if configMap.Kind != "ConfigMap" {
			continue
		}

		found++

		if data == nil {
			data = map[string]string{}
		}

		for key, value := range configMap.Data {
			data[key] = value
		}
	}

	switch {
	case found == 0:
		return nil, false, nil
	case found != documents:
		return nil, false, errors.New(
			"file mixes ConfigMap manifests with other documents; supply either a ConfigMap or a bare overrides document")
	}

	return data, true, nil
}

func sortedKeys(data map[string]string) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
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
