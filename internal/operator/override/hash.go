// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// applyExtraArgs appends arguments to containers after the merge.
//
// This runs last because a patch may legitimately replace args wholesale, and
// extraArgs is defined to append to whatever the patch left behind. That order
// is what makes the precedence predictable rather than dependent on which
// contributor is processed first.
func applyExtraArgs(workload *unstructured.Unstructured, contributors []SourcedEntry) error {
	appendsByContainer := map[string][]any{}

	for _, contributor := range contributors {
		names := make([]string, 0, len(contributor.Entry.ExtraArgs))
		for name := range contributor.Entry.ExtraArgs {
			names = append(names, name)
		}

		sort.Strings(names)

		for _, name := range names {
			for _, arg := range contributor.Entry.ExtraArgs[name] {
				appendsByContainer[name] = append(appendsByContainer[name], arg)
			}
		}
	}

	if len(appendsByContainer) == 0 {
		return nil
	}

	for _, field := range []string{"containers", "initContainers"} {
		containers := nestedSlice(workload.Object, "spec", "template", "spec", field)
		if containers == nil {
			continue
		}

		changed := false

		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				continue
			}

			name, _ := container["name"].(string) //nolint:errcheck // absent means unnamed

			extra, wanted := appendsByContainer[name]
			if !wanted {
				continue
			}

			existing, _ := container["args"].([]any) //nolint:errcheck // absent means no args
			container["args"] = append(append([]any{}, existing...), extra...)
			changed = true
		}

		if !changed {
			continue
		}

		if err := setNestedSlice(workload.Object, containers, "spec", "template", "spec", field); err != nil {
			return fmt.Errorf("append extraArgs to %s: %w", field, err)
		}
	}

	return nil
}

// contributorHash is the canonical hash of the entries that resolved to one
// workload.
//
// Both the applied hash written to the workload and the desired hash published
// in status are computed by this function over the same contributor set, so the
// two are comparable. An earlier revision hashed a per-object applied set
// against a desired hash covering the whole ConfigMap, which differ whenever a
// document targets more than one workload, so the signal was permanently on.
func contributorHash(contributors []SourcedEntry) (string, error) {
	if len(contributors) == 0 {
		return "", nil
	}

	canonical := make([]map[string]any, 0, len(contributors))

	for _, contributor := range contributors {
		canonical = append(canonical, map[string]any{
			"source": contributor.Source.String(),
			"entry":  contributor.Entry,
		})
	}

	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("hash override contributors: %w", err)
	}

	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:]), nil
}

// contributorSources renders the contributor list for the source annotation, so
// an operator reading a live workload can find the entries that shaped it.
func contributorSources(contributors []SourcedEntry) string {
	out := make([]string, 0, len(contributors))
	for _, contributor := range contributors {
		out = append(out, contributor.Source.String())
	}

	return strings.Join(out, ",")
}

// imageDrift reports container image changes a merge introduced.
//
// Overriding an image breaks the version-lockstep invariant: components are
// otherwise pinned to the operator's own version, and a pinned image survives
// operator upgrades indefinitely. That is the likeliest cause of an install
// behaving unlike its reported version, so it is surfaced rather than blocked.
func imageDrift(before, after *unstructured.Unstructured) string {
	var drift []string

	for _, field := range []string{"initContainers", "containers"} {
		original := containerImages(before, field)

		for name, image := range containerImages(after, field) {
			if previous, existed := original[name]; existed && previous != image {
				drift = append(drift, name+"="+image)
			}
		}
	}

	sort.Strings(drift)

	return strings.Join(drift, ",")
}

func containerImages(workload *unstructured.Unstructured, field string) map[string]string {
	out := map[string]string{}

	containers := nestedSlice(workload.Object, "spec", "template", "spec", field)

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		name, _ := container["name"].(string)   //nolint:errcheck // absent means unnamed
		image, _ := container["image"].(string) //nolint:errcheck // absent means no image

		if name != "" {
			out[name] = image
		}
	}

	return out
}
