// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"strings"
	"testing"
)

func TestChairKubeconfigSurfaces(t *testing.T) {
	c := NewDefault()
	if err := c.LoadYAML(strings.NewReader("chair_kubeconfig: /fixture/yaml\n")); err != nil || c.ChairKubeconfig != "/fixture/yaml" {
		t.Fatalf("YAML chair kubeconfig = %q, %v", c.ChairKubeconfig, err)
	}

	if err := c.LoadEnv(func(key string) string {
		switch key {
		case "GANTRY_CHAIR_KUBECONFIG":
			return "/fixture/env"
		case "GANTRY_MEMBERS_KUBECONFIG":
			return "/fixture/obsolete"
		default:
			return ""
		}
	}); err != nil || c.ChairKubeconfig != "/fixture/env" {
		t.Fatalf("env chair kubeconfig = %q, %v", c.ChairKubeconfig, err)
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c.BindFlags(fs)

	if fs.Lookup("members-kubeconfig") != nil {
		t.Fatal("obsolete flag alias retained")
	}

	if err := fs.Parse([]string{"--chair-kubeconfig=/fixture/flag"}); err != nil || c.ChairKubeconfig != "/fixture/flag" {
		t.Fatalf("flag chair kubeconfig = %q, %v", c.ChairKubeconfig, err)
	}

	if err := c.LoadYAML(strings.NewReader("members_kubeconfig: /fixture/obsolete\n")); err == nil {
		t.Fatal("obsolete YAML alias retained")
	}
}
