// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	versions := makeVersions(string(data), "LIBFABRIC_VERSION", "OPENSSL_VERSION")
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
			License: []notice.License{
				{
					Name: "BSD 2-Clause License",
					Link: "https://github.com/ofiwg/libfabric/blob/v" + versions["LIBFABRIC_VERSION"] + "/COPYING",
				},
				{
					Name: "GNU General Public License, Version 2.0",
					Link: "https://github.com/ofiwg/libfabric/blob/v" + versions["LIBFABRIC_VERSION"] + "/COPYING",
				},
			},
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

func makeVersions(data string, names ...string) map[string]string {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	versions := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		fields := strings.Fields(line)
		if len(fields) == 3 && wanted[fields[0]] && (fields[1] == "?=" || fields[1] == ":=" || fields[1] == "=") {
			versions[fields[0]] = fields[2]
		}
	}

	return versions
}
