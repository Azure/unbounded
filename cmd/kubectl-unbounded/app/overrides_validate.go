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
	"k8s.io/apimachinery/pkg/util/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/override"
	"github.com/Azure/unbounded/internal/unbounded"
)

func overridesValidateCommand() *cobra.Command {
	var (
		files      []string
		namespace  string
		kubeconfig string
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

			c, err := newMachineClientWithKubeconfig(kubeconfig)
			if err != nil {
				return err
			}

			return runOverridesValidateCluster(ctx, c, namespace, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil,
		"Override document to validate, or - for standard input. Repeatable. "+
			"Reads the cluster ConfigMap when omitted.")
	cmd.Flags().StringVar(&namespace, "namespace", unbounded.SystemNamespace(),
		"Namespace holding the overrides ConfigMap")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")

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
		contents, err := readOverrideFile(file)
		if err != nil {
			return nil, err
		}

		wrapped, isConfigMap, err := unwrapConfigMap(contents)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}

		if !isConfigMap {
			key := filepath.Base(file)
			if file == "-" {
				key = "stdin.yaml"
			}

			// The key is what this content would become inside the ConfigMap,
			// and the duplicate check below is built on that premise, so it has
			// to be a key a ConfigMap can actually hold.
			if problems := validation.IsConfigMapKey(key); len(problems) > 0 {
				return nil, fmt.Errorf(
					"%s: %q is not a valid ConfigMap key (%s); rename the file, or wrap the document in a ConfigMap manifest",
					file, key, strings.Join(problems, "; "),
				)
			}

			if err := add(key, string(contents), file); err != nil {
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

// maxOverrideFileBytes bounds a single input file.
//
// The operator's own limits are enforced by override.Parse, but they apply to
// the ConfigMap payload, not to the manifest wrapping it. Without a bound here
// the whole file is read and fully YAML-parsed before Parse ever sees it, so
// `-f /dev/zero` allocates until it is killed. The bound is generous relative
// to the 1 MiB the operator will accept, so it only ever catches a mistake.
const maxOverrideFileBytes = 4 << 20

// readOverrideFile reads one input, accepting `-` for stdin as every other
// kubectl `-f` does, and refusing input too large to be a plausible document.
func readOverrideFile(file string) ([]byte, error) {
	if file == "-" {
		contents, err := io.ReadAll(io.LimitReader(os.Stdin, maxOverrideFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read standard input: %w", err)
		}

		if len(contents) > maxOverrideFileBytes {
			return nil, fmt.Errorf("standard input is over the %d byte limit", maxOverrideFileBytes)
		}

		return contents, nil
	}

	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("read %s: is a directory; name the override documents individually", file)
	}

	if info.Size() > maxOverrideFileBytes {
		return nil, fmt.Errorf("read %s: file is %d bytes, over the %d byte limit",
			file, info.Size(), maxOverrideFileBytes)
	}

	contents, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}

	return contents, nil
}

// unwrapConfigMap returns the data of a ConfigMap manifest, reporting whether
// the input was one at all.
//
// Every YAML document in the file must agree: a file is either a ConfigMap
// manifest or a bare overrides document. A mixture is refused rather than
// half-read, because silently skipping the part that did not fit is how a
// document goes unvalidated while the command prints ok.
func unwrapConfigMap(contents []byte) (map[string]string, bool, error) {
	var (
		data      map[string]string
		found     int
		documents int
	)

	for _, raw := range splitYAMLDocuments(contents) {
		if isBlankDocument(raw) {
			// An empty document, produced by a leading --- or a comment block.
			continue
		}

		documents++

		var configMap corev1.ConfigMap

		// A decode failure here is not evidence of a bare overrides document.
		// Treating it as one discarded the real error and handed the file to
		// the override parser, which then complained that `kind`, `metadata`
		// and `data` are not fields of an internal Go type: exactly the
		// diagnostic this function exists to remove. Only a document that
		// decodes and is not a ConfigMap is the bare-document case.
		if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(&configMap); err != nil {
			if isConfigMapDocument(raw) {
				return nil, false, fmt.Errorf("document %d is a ConfigMap but did not decode: %w", documents, err)
			}

			return nil, false, nil
		}

		if configMap.Kind != "ConfigMap" {
			continue
		}

		if configMap.APIVersion != "" && configMap.APIVersion != "v1" {
			return nil, false, fmt.Errorf(
				"document %d has kind ConfigMap with apiVersion %q; the operator reads a core v1 ConfigMap",
				documents, configMap.APIVersion,
			)
		}

		found++

		if data == nil {
			data = map[string]string{}
		}

		for _, key := range sortedKeys(configMap.Data) {
			// Two ConfigMap documents in one file can name the same key, and
			// merging them last-write-wins meant the earlier document was
			// never validated while the command still printed ok. The
			// cross-file check below cannot see this, because by the time it
			// runs the duplicate no longer exists.
			if _, clash := data[key]; clash {
				return nil, false, fmt.Errorf(
					"more than one ConfigMap in this file supplies the key %q; "+
						"a ConfigMap holds one value per key, so rename one of them", key,
				)
			}

			data[key] = configMap.Data[key]
		}
	}

	switch {
	case found == 0:
		return nil, false, nil
	case found != documents:
		return nil, false, errors.New(
			"file mixes ConfigMap manifests with other documents; supply either a ConfigMap or a bare overrides document",
		)
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

// splitYAMLDocuments splits a stream on `---` separators so each document can
// be decoded, and failed against, on its own.
//
// Decoding the stream with one decoder cannot do this: a failure part-way
// through gives no way to say which document failed, and no way to tell a
// document that is not a ConfigMap from one that is malformed.
func splitYAMLDocuments(contents []byte) [][]byte {
	var (
		documents [][]byte
		current   []byte
	)

	for _, line := range bytes.Split(contents, []byte("\n")) {
		trimmed := bytes.TrimRight(line, " \t\r")

		if bytes.Equal(trimmed, []byte("---")) {
			documents = append(documents, current)
			current = nil

			continue
		}

		current = append(current, line...)
		current = append(current, '\n')
	}

	return append(documents, current)
}

// isBlankDocument reports whether a document carries nothing but whitespace and
// comments, which is what a leading `---` or a license header produces.
func isBlankDocument(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] != '#' {
			return false
		}
	}

	return true
}

// isConfigMapDocument reports whether a document claims to be a ConfigMap, used
// to decide whether a decode failure is a broken ConfigMap or simply a document
// of another shape.
//
// It reads the raw text rather than the decoded object precisely because the
// decode is what failed.
func isConfigMapDocument(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.Equal(bytes.TrimSpace(line), []byte("kind: ConfigMap")) {
			return true
		}
	}

	return false
}

func runOverridesValidateCluster(ctx context.Context, c client.Client, namespace string, out io.Writer) error {
	configMap, found, err := getOverridesConfigMap(ctx, c, namespace)
	if err != nil {
		return err
	}

	if !found {
		fprintf(out, "No overrides ConfigMap found in namespace %q.\n", namespace)
		fprintln(out, "If the operator is installed elsewhere, pass --namespace.")

		return nil
	}

	return reportValidation(configMap.Data, out)
}

func reportValidation(data map[string]string, out io.Writer) error {
	entries, problems, err := override.Parse(data)
	if err != nil {
		return err
	}

	problems = append(problems, override.Validate(entries)...)

	if len(problems) > 0 {
		return override.ProblemsError(problems)
	}

	// Reporting ok for input nothing was read from is a false positive: the
	// user named a file, and "0 entries" is far likelier to mean the content
	// did not arrive than that they meant to declare nothing.
	if len(entries) == 0 {
		return errors.New("no override entries found; the input declares nothing to apply")
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
