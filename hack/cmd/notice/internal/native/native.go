// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package native implements a notice.Collector for native dependencies whose
// source versions are pinned in the project Makefile.
package native

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
)

// Collector collects the pinned libfabric and OpenSSL source dependencies.
type Collector struct{}

// New constructs a Collector.
func New() *Collector { return &Collector{} }

// Name implements notice.Collector.
func (c *Collector) Name() string { return "native" }

// Precheck implements notice.Collector.
func (c *Collector) Precheck(root string) error {
	if _, err := os.Stat(filepath.Join(root, "Makefile")); err != nil {
		return fmt.Errorf("stat Makefile: %w", err)
	}

	return nil
}

// Collect implements notice.Collector.
func (c *Collector) Collect(root string) ([]notice.Entry, error) {
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, fmt.Errorf("reading Makefile: %w", err)
	}

	versions, err := makeVersions(string(data), "LIBFABRIC_VERSION", "OPENSSL_VERSION")
	if err != nil {
		return nil, fmt.Errorf("parsing Makefile: %w", err)
	}

	if len(versions) == 0 {
		return nil, nil
	}

	for _, name := range []string{"LIBFABRIC_VERSION", "OPENSSL_VERSION"} {
		if versions[name] == "" {
			return nil, fmt.Errorf("%s pin not found in Makefile", name)
		}
	}

	return []notice.Entry{
		{
			Dependency: "libfabric",
			Ecosystem:  c.Name(),
			Copyright: []string{
				"Copyright (c) Intel Corporation. All rights reserved.",
				"Copyright (c) 2015-2019 Cisco Systems, Inc. All rights reserved.",
			},
			// Upstream offers BSD-2-Clause or GPL-2.0; this project selects BSD-2-Clause.
			License: []notice.License{{
				Name: "BSD 2-Clause License",
				Link: "https://github.com/ofiwg/libfabric/blob/v" + versions["LIBFABRIC_VERSION"] + "/COPYING",
			}},
		},
		{
			Dependency: "OpenSSL",
			Ecosystem:  c.Name(),
			Copyright: []string{
				"Copyright (c) 1998-2025 The OpenSSL Project Authors",
				"Copyright (c) 1995-1998 Eric A. Young, Tim J. Hudson",
			},
			License: []notice.License{{
				Name: "Apache License, Version 2.0",
				Link: "https://github.com/openssl/openssl/blob/openssl-" + versions["OPENSSL_VERSION"] + "/LICENSE.txt",
			}},
		},
	}, nil
}

func makeVersions(data string, names ...string) (map[string]string, error) {
	versions := map[string]string{}

	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		for _, name := range names {
			if !strings.HasPrefix(line, name) {
				continue
			}

			rest := strings.TrimPrefix(line, name)
			// Do not mistake a longer variable name for a pin declaration.
			if rest != "" && !strings.ContainsRune(" \t?:=+!", rune(rest[0])) {
				continue
			}

			rest = strings.TrimSpace(rest)
			value := ""

			for _, operator := range []string{"?=", ":=", "="} {
				if strings.HasPrefix(rest, operator) {
					value = strings.TrimSpace(strings.TrimPrefix(rest, operator))
					break
				}
			}

			if len(strings.Fields(value)) != 1 || strings.ContainsAny(value, "$=:?+!\\") {
				return nil, fmt.Errorf("invalid %s pin declaration %q", name, line)
			}

			versions[name] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning version pins: %w", err)
	}

	return versions, nil
}
