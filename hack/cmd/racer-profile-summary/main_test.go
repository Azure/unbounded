// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/pprof/profile"
)

func fixture() *profile.Profile {
	f1 := &profile.Function{ID: 1, SystemName: "kernel_one"}
	f2 := &profile.Function{ID: 2, SystemName: "kernel_two"}
	m := &profile.Mapping{ID: 1, File: "[kernel]", BuildID: "kernel-build"}
	l1 := &profile.Location{ID: 1, Mapping: m, Line: []profile.Line{{Function: f1}}}
	l2 := &profile.Location{ID: 2, Mapping: m, Line: []profile.Line{{Function: f2}}}

	return &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Function:   []*profile.Function{f1, f2}, Mapping: []*profile.Mapping{m}, Location: []*profile.Location{l1, l2},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{l1, l2, l1}, Value: []int64{999, 10}, Label: map[string][]string{"comm": {"worker"}}},
			{Location: []*profile.Location{l2}, Value: []int64{888, 20}},
		},
	}
}

func TestDistinctSystemNamesAndRecursiveDedup(t *testing.T) {
	p := fixture()

	s, err := summarize(p, "cpu")
	if err != nil {
		t.Fatal(err)
	}

	if s.TotalNS != 30 || s.SampleIndex != 1 || len(s.Flat) != 2 {
		t.Fatalf("unexpected totals: %+v", s)
	}

	if s.Flat[0].Name != "kernel_two" || s.Flat[0].FlatNS != 20 || s.Flat[0].CumulativeNS != 30 {
		t.Fatalf("bad leader: %+v", s.Flat[0])
	}

	if s.Flat[1].Name != "kernel_one" || s.Flat[1].CumulativeNS != 10 {
		t.Fatalf("recursive sample counted twice: %+v", s.Flat[1])
	}

	if len(s.LabelGroups) != 2 || p.Function[0].Name != "kernel_one" {
		t.Fatal("labels or normalization lost")
	}
}

func TestMappingAndUnsymbolizedIdentity(t *testing.T) {
	p := fixture()
	m := &profile.Mapping{ID: 2, File: "module.ko"}
	p.Mapping = append(p.Mapping, m)
	l := &profile.Location{ID: 3, Mapping: m, Line: p.Location[0].Line}
	u := &profile.Location{ID: 4}
	v := &profile.Location{ID: 5}
	p.Location = append(p.Location, l, u, v)
	p.Sample = append(p.Sample, &profile.Sample{Location: []*profile.Location{l, u, v}, Value: []int64{1, 5}}, &profile.Sample{Value: []int64{1, 7}})

	s, err := summarize(p, "cpu")
	if err != nil {
		t.Fatal(err)
	}

	if len(s.Flat) != 5 || s.TotalNS != 42 || s.UnattributedNS != 7 {
		t.Fatalf("identity merged: %+v", s)
	}
}

func TestSelectionErrors(t *testing.T) {
	for _, name := range []string{"missing", "wrong-unit", "ambiguous", "short-values", "negative", "overflow"} {
		t.Run(name, func(t *testing.T) {
			p := fixture()

			switch name {
			case "missing":
				p.SampleType[1].Type = "wall"
			case "wrong-unit":
				p.SampleType[1].Unit = "count"
			case "ambiguous":
				p.SampleType[0] = p.SampleType[1]
			case "short-values":
				p.Sample[0].Value = nil
			case "negative":
				p.Sample[0].Value[1] = -1
			case "overflow":
				p.Sample[0].Value[1] = math.MaxInt64
			}

			if _, err := summarize(p, "cpu"); err == nil {
				t.Fatal("expected error")
			}
		})
	}

	p := fixture()

	p.SampleType[1].Type = "kernel_cpu"
	if _, err := summarize(p, "kernel_cpu"); err != nil {
		t.Fatal(err)
	}
}

func TestCLIAndNormalizedRoundTrip(t *testing.T) {
	var raw bytes.Buffer
	if err := fixture().Write(&raw); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	input, normalized := filepath.Join(dir, "input.pprof"), filepath.Join(dir, "normalized.pprof")
	if err := os.WriteFile(input, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"-normalized", normalized}, {"-input", input}} {
		var out bytes.Buffer
		if err := run(args, bytes.NewReader(raw.Bytes()), &out); err != nil {
			t.Fatal(err)
		}

		var s summary
		if err := json.Unmarshal(out.Bytes(), &s); err != nil {
			t.Fatal(err)
		}

		if s.TotalNS != 30 || len(s.Flat) != 2 {
			t.Fatalf("bad JSON: %s", out.String())
		}
	}

	b, err := os.ReadFile(normalized)
	if err != nil {
		t.Fatal(err)
	}

	p, err := profile.ParseData(b)
	if err != nil {
		t.Fatal(err)
	}

	if p.Function[0].Name != "kernel_one" || p.Function[1].Name != "kernel_two" || p.Sample[0].Label["comm"][0] != "worker" {
		t.Fatal("normalized profile lost symbols or labels")
	}

	if err := run(nil, bytes.NewBufferString("bad profile"), &bytes.Buffer{}); err == nil {
		t.Fatal("expected parse failure")
	}

	if err := run([]string{"-input", filepath.Join(dir, "missing")}, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("expected open failure")
	}
}

func TestNormalizedNeverOverwrites(t *testing.T) {
	var raw bytes.Buffer
	if err := fixture().Write(&raw); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"same-path", "symlink", "hard-link", "existing-output"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "raw.pprof")
			output := filepath.Join(dir, "normalized.pprof")

			if err := os.WriteFile(input, raw.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}

			switch name {
			case "same-path":
				output = input
			case "symlink":
				if err := os.Symlink(input, output); err != nil {
					t.Fatal(err)
				}
			case "hard-link":
				if err := os.Link(input, output); err != nil {
					t.Fatal(err)
				}
			case "existing-output":
				if err := os.WriteFile(output, raw.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var out bytes.Buffer

			err := run([]string{"-input", input, "-normalized", output}, nil, &out)
			if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
				t.Fatalf("expected explicit overwrite refusal, got %v", err)
			}

			if out.Len() != 0 {
				t.Fatal("emitted summary after failed export")
			}

			for _, path := range []string{input, output} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}

				if !bytes.Equal(data, raw.Bytes()) {
					t.Fatalf("profile modified at %s", path)
				}
			}
		})
	}
}
