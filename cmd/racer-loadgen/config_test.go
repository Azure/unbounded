// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	got, err := parseConfig([]string{"-seed", "42", "-cache-name=loadgen"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	want := config{
		mode: "racer", registryListen: ":8081", gantryEndpoint: "http://127.0.0.1:5000", layersPerImage: 4, layerConcurrency: 3,
		endpoint: "/run/racer/loadgen/client/socket", originSocket: "/run/racer/loadgen/origin/socket", listen: ":8080",
		footprint: 512_000_000_000, objectSize: 1_000_000_000, seed: 42, exponent: 1,
		concurrency: 4, pageConcurrency: 8, timeout: 5 * time.Minute, ttl: time.Hour,
		gantryReadyTimeout: 10 * time.Minute,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
}

func TestParseSizeUnitsAndBounds(t *testing.T) {
	valid := map[string]int64{
		"1": 1, "17B": 17, "2kb": 2000, "3MB": 3_000_000,
		"4GB": 4_000_000_000, "5TB": 5_000_000_000_000,
		"2KiB": 2 << 10, "3mib": 3 << 20, "4GiB": 4 << 30, "5TiB": 5 << 40,
		"9223372036854775807": math.MaxInt64, "9223372036854775KB": 9_223_372_036_854_775_000,
	}
	for text, want := range valid {
		t.Run(text, func(t *testing.T) {
			got, err := parseSize(text)
			if err != nil || got != want {
				t.Fatalf("parseSize(%q) = %d, %v; want %d", text, got, err, want)
			}
		})
	}

	for _, text := range []string{"", "B", "0", "0GB", "-1", "+1", "1.5GB", "1e3", " 1GB", "1 GB", "1GB ", "1PB", "9223372036854775808", "9223372036854776KB", "8388608TiB"} {
		t.Run("invalid_"+text, func(t *testing.T) {
			if _, err := parseSize(text); err == nil {
				t.Fatalf("parseSize(%q) succeeded", text)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	for _, args := range [][]string{
		{"-exponent", "-0.1"},
		{"-exponent", "NaN"},
		{"-exponent", "+Inf"},
		{"-exponent", "-Inf"},
		{"-footprint", "3GB", "-object-size", "2GB"},
		{"-footprint", "1GB", "-object-size", "2GB"},
		{"-footprint", "1000001", "-object-size", "1"},
		{"-footprint", "0"},
		{"-object-size", "0"},
		{"-concurrency", "0"},
		{"-concurrency", "-1"},
		{"-page-concurrency", "0"},
		{"-page-concurrency", "-1"},
		{"-timeout", "0"},
		{"-timeout", "-1s"},
		{"-ttl", "-1s"},
		{"-duration", "-1s"},
		{"-seed", "9223372036854775808"},
		{"-seed", "abc"},
		{"-timeout", "tomorrow"},
		{"unexpected"},
		{"-unknown"},
	} {
		t.Run("invalid_"+args[0]+"_"+args[len(args)-1], func(t *testing.T) {
			if _, err := parseConfig(append([]string{"-cache-name=loadgen"}, args...), io.Discard); err == nil {
				t.Fatalf("parseConfig(%q) succeeded", args)
			}
		})
	}

	for _, args := range [][]string{
		{"-footprint", "1000000", "-object-size", "1", "-exponent", "0"},
		{"-footprint", "1GiB", "-object-size", "1GiB", "-exponent", "1.7976931348623157e308"},
		{"-ttl", "0", "-duration", "0", "-timeout", "1ns", "-concurrency", "1", "-page-concurrency", "1"},
		{"-seed", "-9223372036854775808"},
		{"-seed", "9223372036854775807"},
	} {
		if _, err := parseConfig(append([]string{"-cache-name=loadgen"}, args...), io.Discard); err != nil {
			t.Errorf("parseConfig(%q): %v", args, err)
		}
	}
}

func TestConfigCacheIdentity(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-cache-name="},
		{"-cache-name=../unsafe"},
		{"-cache-name=UPPER"},
		{"-endpoint=/run/racer/test/client/socket"},
		{"-origin-socket=/run/racer/test/origin/socket"},
		{"-cache-uid=legacy"},
		{"-cache-name=example", "-endpoint=/run/racer/other/client/socket"},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Fatalf("accepted missing or ambiguous identity: %v", args)
		}
	}

	c, err := parseConfig([]string{"-endpoint=/run/racer/actual/client/socket", "-origin-socket=/run/racer/actual/origin/socket"}, io.Discard)
	if err != nil || c.endpoint != "/run/racer/actual/client/socket" || c.originSocket != "/run/racer/actual/origin/socket" {
		t.Fatalf("explicit status endpoints: %+v, %v", c, err)
	}
}
