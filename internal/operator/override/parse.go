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
	"time"

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
// Keys are independent, and a key that fails does not stop the others being
// read. That is the whole reason the format is one document per key: splitting
// by concern or by team is only useful if a mistake in one key is contained to
// it. The returned problems say which key or which entry failed, so the caller
// can withhold the workloads that are actually in doubt rather than all of
// them.
//
// The returned error is reserved for a failure of the payload as a whole, where
// nothing can be read and no attribution is possible.
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
func Parse(data map[string]string) ([]SourcedEntry, []Problem, error) {
	keys := make([]string, 0, len(data))

	for key := range data {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	total := 0
	for _, key := range keys {
		total += len(data[key])
	}

	if total > maxTotalBytes {
		// A payload-level failure, not a key-level one: the limit is on every
		// value together, so no individual key is at fault and none can be
		// read in isolation.
		return nil, nil, fmt.Errorf(
			"overrides are %d bytes in total, over the %d byte limit", total, maxTotalBytes,
		)
	}

	var (
		entries  []SourcedEntry
		problems []Problem
	)

	for _, key := range keys {
		if strings.TrimSpace(data[key]) == "" {
			continue
		}

		doc, err := parseDocument(key, data[key])
		if err != nil {
			problems = append(problems, keyProblem(key, err))

			continue
		}

		if len(doc.Overrides) > maxEntries {
			problems = append(problems, keyProblem(key, fmt.Errorf(
				"%d entries, over the %d entry limit", len(doc.Overrides), maxEntries,
			)))

			continue
		}

		keyEntries, keyProblems := parseEntries(key, doc)

		entries = append(entries, keyEntries...)
		problems = append(problems, keyProblems...)
	}

	return entries, problems, nil
}

// parseEntries normalizes one document's entries, attributing a failure to the
// entry that caused it.
//
// A bad patch in entry three does not put entries one and two in doubt: the
// component, kind and Sites of each decode before the patch is normalized, so
// the failure can name exactly what it would have touched.
func parseEntries(key string, doc Document) ([]SourcedEntry, []Problem) {
	var (
		entries  []SourcedEntry
		problems []Problem
	)

	for i, entry := range doc.Overrides {
		source := Source{Key: key, Index: i}

		normalized, err := normalizeJSON(entry.Patch, "patch")
		if err != nil {
			problems = append(problems, entryProblem(source, entry, err))

			continue
		}

		patch, ok := normalized.(map[string]any)
		if !ok && normalized != nil {
			problems = append(problems, entryProblem(source, entry, errPatchNotAMapping))

			continue
		}

		entry.Patch = patch
		entries = append(entries, SourcedEntry{Entry: entry, Source: source})
	}

	return entries, problems
}

// errPatchNotAMapping is returned when a patch decodes to something other than
// a mapping, which no strategic merge patch can be.
var errPatchNotAMapping = errors.New("patch must be a mapping")

// Limits on what a single pass will parse.
//
// These are not arbitrary. yaml.v3 checks for duplicate mapping keys with a
// nested loop over every pair (decode.go, d.mapping), and that check is on by
// default. The cost is quadratic in the number of keys in one mapping, and the
// operator reconciles with a single worker, so a document that is legal in
// every other respect can pin the whole controller.
//
// Measured with yaml.v3 v3.0.1, decoding one mapping of N annotation keys:
//
//	N=1,000   30 KB     4ms
//	N=4,000   123 KB   37ms
//	N=16,000  501 KB  498ms
//	N=40,000  1.2 MB  4.1s
//
// The apiserver's own ~1 MiB ConfigMap limit is therefore not a bound on the
// work: it permits several seconds of CPU per pass, repeated on every pass.
// Parsing into a yaml.Node does not run that check and stays linear (58ms for
// the 1.2 MB case), which is why the structural limits below are enforced on
// the node tree before the strict decode is allowed to run.
const (
	// maxDocumentBytes bounds one ConfigMap value.
	maxDocumentBytes = 256 << 10

	// maxTotalBytes bounds every value in the ConfigMap together.
	maxTotalBytes = 1 << 20

	// maxEntries bounds the overrides list in one document.
	maxEntries = 256

	// maxMappingKeys bounds one mapping, which is what the quadratic check
	// runs over. At this size the check costs single-digit milliseconds.
	maxMappingKeys = 1024

	// maxNodes bounds the whole document, so many small mappings cannot add up
	// to the same cost that one large mapping is forbidden from reaching.
	maxNodes = 20000

	// maxDepth bounds nesting, which recursive walks in this package descend.
	maxDepth = 32
)

// parseDocument parses one ConfigMap value.
func parseDocument(key, raw string) (Document, error) {
	if len(raw) > maxDocumentBytes {
		return Document{}, fmt.Errorf(
			"document is %d bytes, over the %d byte limit; split it across ConfigMap keys",
			len(raw), maxDocumentBytes,
		)
	}

	if err := checkStructure(raw); err != nil {
		return Document{}, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.KnownFields(true)

	var doc Document

	if err := decoder.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return Document{}, errors.New("document is empty")
		}

		return Document{}, err
	}

	// A second successful decode means a second document, or trailing content
	// after a `---` separator. Silently ignoring the remainder would apply half
	// of what the user wrote.
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Document{}, err
		}

		return Document{}, errors.New(
			"contains more than one YAML document; put one document per key",
		)
	}

	if doc.APIVersion == "" {
		return Document{}, fmt.Errorf(
			"apiVersion is required and must be %q", APIVersion,
		)
	}

	if doc.APIVersion != APIVersion {
		return Document{}, fmt.Errorf(
			"unsupported apiVersion %q, want %q", doc.APIVersion, APIVersion,
		)
	}

	return doc, nil
}

// checkStructure walks the raw YAML node tree, rejecting merge keys and
// anything past the structural limits.
//
// Parsing into a yaml.Node is linear, so this runs before the strict decode
// and is what keeps that decode's quadratic duplicate-key check bounded.
//
// Merge keys are rejected because yaml.v3 expands them silently, which would
// let content reach the merge that the allowlist walker never inspected: it
// only ever sees the expanded result rather than the alias that produced it.
func checkStructure(raw string) error {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		// A parse error here will be reported with better context by the strict
		// decode that follows.
		return nil //nolint:nilerr // deliberate: defer to the strict decoder
	}

	nodes := 0

	return walkNodeDepth(&root, 0, func(n *yaml.Node, depth int) error {
		nodes++

		if nodes > maxNodes {
			return fmt.Errorf(
				"document has more than %d nodes; split it across ConfigMap keys", maxNodes,
			)
		}

		if depth > maxDepth {
			return fmt.Errorf("document nests deeper than %d levels at line %d",
				maxDepth, n.Line)
		}

		if n.Kind != yaml.MappingNode {
			return nil
		}

		if keys := len(n.Content) / 2; keys > maxMappingKeys {
			return fmt.Errorf(
				"mapping at line %d has %d keys, over the %d key limit; "+
					"duplicate-key checking is quadratic, so one large mapping can stall reconciliation",
				n.Line, keys, maxMappingKeys,
			)
		}

		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == mergeKey || n.Content[i].Tag == "!!merge" {
				return fmt.Errorf(
					"YAML merge keys (%s) are not supported at line %d; write the document out in full",
					mergeKey, n.Content[i].Line,
				)
			}
		}

		return nil
	})
}

// walkNode visits every node in a YAML tree.
func walkNodeDepth(n *yaml.Node, depth int, visit func(*yaml.Node, int) error) error {
	if n == nil {
		return nil
	}

	if err := visit(n, depth); err != nil {
		return err
	}

	for _, child := range n.Content {
		if err := walkNodeDepth(child, depth+1, visit); err != nil {
			return err
		}
	}

	return nil
}

// normalizeJSON converts decoded YAML values to the types the Kubernetes
// helpers accept, and rejects anything that cannot be represented.
//
// Two things make this necessary rather than pedantic. yaml.v3 decodes whole
// numbers as int, but k8s.io/apimachinery's DeepCopyJSONValue accepts only
// int64 and panics on anything else. It also resolves unquoted dates to
// time.Time, which panics the same way, so `nodeSelector: {k: 2026-08-11}`
// would take the operator down rather than produce an error.
//
// Timestamps are rejected rather than coerced. Rewriting one to RFC3339 would
// silently produce a value the user did not write; almost always they meant a
// string and forgot to quote it, and saying so is more useful than guessing.
//
// Normalizing here rather than at each use keeps validation, merging and
// hashing working on one representation.
func normalizeJSON(value any, path string) (any, error) {
	switch typed := value.(type) {
	case nil:
		// Explicit nulls are rejected later, with a message that explains why
		// deletion is not available.
		return nil, nil

	case map[string]any:
		out := make(map[string]any, len(typed))

		for key, element := range typed {
			normalized, err := normalizeJSON(element, joinPath(path, key))
			if err != nil {
				return nil, err
			}

			out[key] = normalized
		}

		return out, nil

	case map[any]any:
		// Defensive: a decode into an untyped map can yield this shape.
		out := make(map[string]any, len(typed))

		for key, element := range typed {
			name := fmt.Sprintf("%v", key)

			normalized, err := normalizeJSON(element, joinPath(path, name))
			if err != nil {
				return nil, err
			}

			out[name] = normalized
		}

		return out, nil

	case []any:
		out := make([]any, len(typed))

		for i, element := range typed {
			normalized, err := normalizeJSON(element, path)
			if err != nil {
				return nil, err
			}

			out[i] = normalized
		}

		return out, nil

	case string, bool, int64, float64:
		return value, nil

	case int:
		return int64(typed), nil
	case int8:
		return int64(typed), nil
	case int16:
		return int64(typed), nil
	case int32:
		return int64(typed), nil
	case uint:
		return int64(typed), nil
	case uint8:
		return int64(typed), nil
	case uint16:
		return int64(typed), nil
	case uint32:
		return int64(typed), nil
	case uint64:
		return int64(typed), nil
	case float32:
		return float64(typed), nil

	case time.Time:
		return nil, fmt.Errorf(
			"%s holds a YAML timestamp (%s); quote it if you meant the string %q",
			describePath(path), typed.Format(time.RFC3339), typed.Format("2006-01-02"),
		)

	default:
		return nil, fmt.Errorf("%s holds an unsupported YAML type %T", describePath(path), value)
	}
}
