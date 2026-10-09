// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
)

const maxNodeCapsBytes = 64 << 10

var nodeNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func validNodeName(name string) bool {
	return len(name) <= 253 && nodeNamePattern.MatchString(name)
}

func validateNodeCapsOptions(opts pullOptions) error {
	if opts.NodeCapsFile == "" {
		return nil
	}

	if opts.ConcurrencyFile == "" || !validNodeName(opts.NodeName) ||
		filepath.Dir(opts.ConcurrencyFile) != filepath.Dir(opts.NodeCapsFile) ||
		filepath.Clean(opts.ConcurrencyFile) == filepath.Clean(opts.NodeCapsFile) {
		return errors.New("node caps require an exact node-name and distinct keys in the same projected ConfigMap directory")
	}

	return nil
}

// object rejects duplicate keys before decoding values. JSON's default map
// decoding silently accepts duplicates, which is unsafe for control authority.
func controlObject(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))

	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}

	result := make(map[string]json.RawMessage)

	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}

		key, ok := token.(string)
		if !ok {
			return nil, errors.New("expected object key")
		}

		if _, exists := result[key]; exists {
			return nil, errors.New("duplicate control key")
		}

		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}

		result[key] = value
		if len(result) > 256 {
			return nil, errors.New("too many control entries")
		}
	}

	if _, err := d.Token(); err != nil {
		return nil, err
	}

	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}

	return result, nil
}

func parseNodeCap(data []byte, node string) (int, error) {
	if len(data) > maxNodeCapsBytes {
		return 0, errors.New("node caps exceed size limit")
	}

	obj, err := controlObject(data)
	if err != nil {
		return 0, err
	}

	if len(obj) != 2 || string(obj["version"]) != "1" || obj["caps"] == nil {
		return 0, errors.New("node caps require version 1 and caps only")
	}

	caps, err := controlObject(obj["caps"])
	if err != nil {
		return 0, err
	}

	selected := maxLiveConcurrency

	for name, raw := range caps {
		var n int

		if !validNodeName(name) || bytes.Equal(raw, []byte("null")) {
			return 0, errors.New("invalid node cap")
		}

		if err := json.Unmarshal(raw, &n); err != nil || n < 0 || n > maxLiveConcurrency {
			return 0, errors.New("node cap must be an integer in [0, 256]")
		}

		if name == node {
			selected = n
		}
	}

	return selected, nil
}

type nodeCapState struct {
	global, cap, effective int
}

func (s *nodeCapState) poll(opts pullOptions) (int, error) {
	previous := *s
	globalPath := opts.ConcurrencyFile
	capPath := opts.NodeCapsFile
	// Resolve the generation once. Old-generation deletion causes a safe read
	// failure rather than mixing global and cap keys from different generations.
	generation, projectionErr := filepath.EvalSymlinks(filepath.Join(filepath.Dir(globalPath), "..data"))
	if projectionErr == nil {
		globalPath = filepath.Join(generation, filepath.Base(globalPath))
		capPath = filepath.Join(generation, filepath.Base(capPath))
	}

	global, globalErr := readConcurrency(globalPath)
	if globalErr == nil {
		s.global = global
	} else if latest, err := readConcurrency(opts.ConcurrencyFile); err == nil {
		// If kubelet removed the pinned generation, a fresh scalar may only
		// lower authority. In particular, a newly projected C0 still wins.
		s.global = min(s.global, latest)
	}

	var capErr error

	capValue := s.cap

	if projectionErr != nil {
		capErr = fmt.Errorf("node caps require ConfigMap ..data projection: %w", projectionErr)
	} else {
		data, err := readControlFile(capPath, maxNodeCapsBytes)

		capErr = err
		if err == nil {
			capValue, capErr = parseNodeCap(data, opts.NodeName)
		}
	}

	err := errors.Join(globalErr, capErr)
	if err == nil {
		s.cap = capValue
		s.effective = min(s.global, s.cap)
	} else {
		if capErr == nil {
			s.cap = min(s.cap, capValue)
		}

		// Initial effective is zero. Errors can never resume a paused reader or
		// increase its admission, even if the global value has increased.
		s.effective = min(s.effective, s.global, s.cap)
	}

	if previous != *s {
		slog.Info("node concurrency selected", "node", opts.NodeName, "global", s.global, "cap", s.cap, "effective", s.effective, "error", err)
	}

	return s.effective, err
}
