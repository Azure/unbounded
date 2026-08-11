// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// mergeKey is the YAML merge key. Expanding it would let a document reference
// content the allowlist walker never visits, so it is rejected rather than
// resolved.
const mergeKey = "<<"

// Source identifies where an entry came from, so every error and every applied
// override can name its origin precisely.
type Source struct {
	// Key is the ConfigMap data key the document was read from.
	Key string

	// Index is the entry's position in that document's overrides list.
	Index int
}

// String renders a source for error messages and annotations.
func (s Source) String() string {
	return fmt.Sprintf("%s[%d]", s.Key, s.Index)
}

// SourcedEntry is an entry paired with where it came from.
type SourcedEntry struct {
	Entry  Entry
	Source Source
}

// Parse reads every key in a ConfigMap payload as an independent overrides
// document and returns the entries in deterministic order: sorted ConfigMap
// key, then position within that key's document.
//
// Parsing is strict by design. The apiVersion gate is only meaningful if the
// document that passes it is the document the user wrote, so anything
// ambiguous is rejected rather than interpreted:
//
//   - unknown fields, at every level including inside the patch
//   - duplicate keys, which most decoders silently resolve to the last value
//   - more than one YAML document per key, and any trailing content
//   - YAML merge keys, which would hide content from validation
//
// An empty or whitespace-only value is ignored rather than rejected, because
// `kubectl create configmap --from-file` on an empty file is a plausible
// accident rather than an intent.
func Parse(data map[string]string) ([]SourcedEntry, error) {
	keys := make([]string, 0, len(data))

	for key := range data {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	var entries []SourcedEntry

	for _, key := range keys {
		if strings.TrimSpace(data[key]) == "" {
			continue
		}

		doc, err := parseDocument(key, data[key])
		if err != nil {
			return nil, err
		}

		for i, entry := range doc.Overrides {
			entries = append(entries, SourcedEntry{Entry: entry, Source: Source{Key: key, Index: i}})
		}
	}

	return entries, nil
}

// parseDocument parses one ConfigMap value.
func parseDocument(key, raw string) (Document, error) {
	if err := rejectMergeKeys(key, raw); err != nil {
		return Document{}, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.KnownFields(true)

	var doc Document

	if err := decoder.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return Document{}, fmt.Errorf("overrides key %q: document is empty", key)
		}

		return Document{}, fmt.Errorf("overrides key %q: %w", key, err)
	}

	// A second successful decode means a second document, or trailing content
	// after a `---` separator. Silently ignoring the remainder would apply half
	// of what the user wrote.
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Document{}, fmt.Errorf("overrides key %q: %w", key, err)
		}

		return Document{}, fmt.Errorf(
			"overrides key %q: contains more than one YAML document; put one document per key", key)
	}

	if doc.APIVersion == "" {
		return Document{}, fmt.Errorf(
			"overrides key %q: apiVersion is required and must be %q", key, APIVersion)
	}

	if doc.APIVersion != APIVersion {
		return Document{}, fmt.Errorf(
			"overrides key %q: unsupported apiVersion %q, want %q", key, doc.APIVersion, APIVersion)
	}

	return doc, nil
}

// rejectMergeKeys walks the raw YAML node tree looking for merge keys.
//
// yaml.v3 expands them silently, which would let content reach the merge that
// the allowlist walker never inspected, because it only ever sees the expanded
// result rather than the alias that produced it.
func rejectMergeKeys(key, raw string) error {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		// A parse error here will be reported with better context by the strict
		// decode that follows.
		return nil //nolint:nilerr // deliberate: defer to the strict decoder
	}

	return walkNode(&root, func(n *yaml.Node) error {
		if n.Kind != yaml.MappingNode {
			return nil
		}

		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == mergeKey || n.Content[i].Tag == "!!merge" {
				return fmt.Errorf(
					"overrides key %q: YAML merge keys (%s) are not supported at line %d; write the document out in full",
					key, mergeKey, n.Content[i].Line)
			}
		}

		return nil
	})
}

// walkNode visits every node in a YAML tree.
func walkNode(n *yaml.Node, visit func(*yaml.Node) error) error {
	if n == nil {
		return nil
	}

	if err := visit(n); err != nil {
		return err
	}

	for _, child := range n.Content {
		if err := walkNode(child, visit); err != nil {
			return err
		}
	}

	return nil
}
