// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	netconfig "github.com/Azure/unbounded/internal/net/config"
)

// images.kubeProxy reaches the managed kube-proxy DaemonSets through the net
// controller, which creates them at runtime and takes their image as a flag on
// its own container. The setting therefore resolves to the net controller
// Deployment, exactly as an entry targeting that Deployment would.
const (
	kubeProxyImageField     = "images.kubeProxy"
	kubeProxyImageComponent = "net"
	kubeProxyImageKind      = "Deployment"
	kubeProxyImageContainer = "controller"

	// kubeProxyImageFlag is the argument the net controller reads the image from.
	kubeProxyImageFlag = "--" + netconfig.ManagedKubeProxyImageFlag
)

// imageEntries turns a document's images block into the entries it stands for.
//
// Representing the setting as an entry is what lets everything downstream treat
// it uniformly: a bad value withholds only the net controller Deployment, the
// value is part of that Deployment's override hash, and Site status reports it
// like any other override of the workload.
func imageEntries(key string, doc Document) []SourcedEntry {
	if doc.Images == nil || doc.Images.KubeProxy == nil {
		return nil
	}

	return []SourcedEntry{{
		Entry: Entry{
			Component:      kubeProxyImageComponent,
			Kind:           kubeProxyImageKind,
			KubeProxyImage: doc.Images.KubeProxy.Image,
		},
		Source: Source{Key: key, Field: kubeProxyImageField},
	}}
}

// validateKubeProxyImage checks the value images.kubeProxy passes to the
// controller with the controller's own parser, so the two cannot disagree about
// what a valid image is or whether it names a version.
func validateKubeProxyImage(image string) []string {
	if image == "" {
		return []string{"image is required"}
	}

	if _, err := netconfig.ParseImageReference(image); err != nil {
		return []string{err.Error()}
	}

	return nil
}

// checkKubeProxyImageContainer verifies that the workload has the container
// images.kubeProxy passes its flag to. Container names are release-specific, so
// a rename has to be reported rather than silently drop the image.
func checkKubeProxyImageContainer(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	if entry.KubeProxyImage == "" || containerNames(workload, "containers")[kubeProxyImageContainer] {
		return nil
	}

	return []error{fmt.Errorf(
		"%s: %s %s has no container %q to pass %s to",
		source, workload.GetKind(), workload.GetName(), kubeProxyImageContainer, kubeProxyImageFlag,
	)}
}

// kubeProxyImageConflicts rejects disagreement about the managed kube-proxy
// image.
//
// Two documents setting different images disagree. So does an entry passing
// the flag itself, through extraArgs or a replaced argument list, alongside
// images.kubeProxy: the controller would honor whichever came last, which is
// precedence by ordering. Two documents setting the same image agree, and the
// flag is passed once.
func kubeProxyImageConflicts(contributors []SourcedEntry) []error {
	var (
		first    *SourcedEntry
		problems []error
	)

	for i := range contributors {
		contributor := &contributors[i]
		if contributor.Entry.KubeProxyImage == "" {
			continue
		}

		if first == nil {
			first = contributor

			continue
		}

		if contributor.Entry.KubeProxyImage != first.Entry.KubeProxyImage {
			problems = append(problems, fmt.Errorf(
				"%s and %s set different kube-proxy images; overrides do not resolve disagreement by ordering",
				first.Source, contributor.Source,
			))
		}
	}

	if first == nil {
		return problems
	}

	for _, contributor := range contributors {
		if passesKubeProxyImageFlag(contributor.Entry) {
			problems = append(problems, fmt.Errorf(
				"%s passes %s to container %q, which %s also sets; set the image in one place",
				contributor.Source, kubeProxyImageFlag, kubeProxyImageContainer, first.Source,
			))
		}
	}

	return problems
}

// passesKubeProxyImageFlag reports whether an entry hands the controller the
// kube-proxy image flag directly, in either of the two ways an override can
// reach its arguments.
func passesKubeProxyImageFlag(entry Entry) bool {
	args := make([]any, 0, len(entry.ExtraArgs[kubeProxyImageContainer]))
	for _, arg := range entry.ExtraArgs[kubeProxyImageContainer] {
		args = append(args, arg)
	}

	for _, container := range patchedContainers(entry.Patch, "containers") {
		if name, _ := container["name"].(string); name != kubeProxyImageContainer { //nolint:errcheck // absent means unnamed
			continue
		}

		if replaced, ok := container["args"].([]any); ok {
			args = append(args, replaced...)
		}
	}

	for _, arg := range args {
		value, ok := arg.(string)
		if ok && (value == kubeProxyImageFlag || strings.HasPrefix(value, kubeProxyImageFlag+"=")) {
			return true
		}
	}

	return false
}

// applyKubeProxyImage passes images.kubeProxy to the controller as its flag.
//
// It runs after extraArgs so the flag survives a patch that replaced the
// argument list. Conflict detection has already rejected any contributor
// passing the flag itself, so running last decides nothing.
func applyKubeProxyImage(workload *unstructured.Unstructured, contributors []SourcedEntry) error {
	image := ""

	for _, contributor := range contributors {
		if contributor.Entry.KubeProxyImage != "" {
			image = contributor.Entry.KubeProxyImage

			break
		}
	}

	if image == "" {
		return nil
	}

	containers := nestedSlice(workload.Object, "spec", "template", "spec", "containers")

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if name, _ := container["name"].(string); name != kubeProxyImageContainer { //nolint:errcheck // absent means unnamed
			continue
		}

		existing, _ := container["args"].([]any) //nolint:errcheck // absent means no args
		container["args"] = append(append([]any{}, existing...), kubeProxyImageFlag+"="+image)

		if err := setNestedSlice(workload.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("pass %s to container %q: %w", kubeProxyImageFlag, kubeProxyImageContainer, err)
		}

		return nil
	}

	// Resolution already checked that the container exists; this is the
	// backstop should that check ever be bypassed.
	return fmt.Errorf("no container %q to pass %s to", kubeProxyImageContainer, kubeProxyImageFlag)
}
