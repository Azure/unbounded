// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// racer-profile-summary summarizes raw CPU pprof without address-based merging.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/google/pprof/profile"
)

type identity struct {
	FunctionID uint64 `json:"function_id"`
	MappingID  uint64 `json:"mapping_id"`
	LocationID uint64 `json:"unsymbolized_location_id,omitempty"`
}

type functionTotal struct {
	identity
	Name         string `json:"name"`
	SystemName   string `json:"system_name,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Module       string `json:"module,omitempty"`
	BuildID      string `json:"build_id,omitempty"`
	FlatNS       int64  `json:"flat_ns"`
	CumulativeNS int64  `json:"cumulative_ns"`
}

type labelTotal struct {
	Labels    map[string][]string `json:"labels,omitempty"`
	NumLabels map[string][]int64  `json:"num_labels,omitempty"`
	NumUnits  map[string][]string `json:"num_units,omitempty"`
	TotalNS   int64               `json:"total_ns"`
	Samples   int                 `json:"samples"`
}

type summary struct {
	SampleType     string          `json:"sample_type"`
	SampleUnit     string          `json:"sample_unit"`
	SampleIndex    int             `json:"sample_index"`
	Samples        int             `json:"samples"`
	TotalNS        int64           `json:"total_ns"`
	UnattributedNS int64           `json:"unattributed_ns"`
	DurationNS     int64           `json:"duration_ns"`
	Flat           []functionTotal `json:"flat"`
	Cumulative     []functionTotal `json:"cumulative"`
	LabelGroups    []labelTotal    `json:"label_groups"`
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) (resultErr error) {
	flags := flag.NewFlagSet("racer-profile-summary", flag.ContinueOnError)
	input := flags.String("input", "-", "raw pprof path, or - for stdin")
	kind := flags.String("sample-type", "cpu", "sample type to select (must use nanoseconds)")

	normalized := flags.String("normalized", "", "new output gzip pprof path with empty function names filled from SystemName (never overwrites)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments; use -input")
	}

	reader := stdin

	if *input != "-" {
		f, err := os.Open(*input)
		if err != nil {
			return err
		}

		defer func() {
			resultErr = errors.Join(resultErr, f.Close())
		}()

		reader = f
	}

	p, err := profile.Parse(reader)
	if err != nil {
		return fmt.Errorf("parse profile: %w", err)
	}

	result, err := summarize(p, *kind)
	if err != nil {
		return err
	}

	if *normalized != "" {
		// Exclusive creation also protects raw input reached through a symlink or
		// hard link, and avoids a check-then-truncate race with another writer.
		f, err := os.OpenFile(*normalized, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create normalized profile (refusing to overwrite any existing path, including raw input): %w", err)
		}

		writeErr := p.Write(f)

		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")

	return encoder.Encode(result)
}

func summarize(p *profile.Profile, kind string) (*summary, error) {
	if err := p.CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid profile: %w", err)
	}

	index := -1

	for i, st := range p.SampleType {
		if st.Type == kind && st.Unit == "nanoseconds" {
			if index != -1 {
				return nil, fmt.Errorf("ambiguous sample type %q/nanoseconds", kind)
			}

			index = i
		}
	}

	if index < 0 {
		return nil, fmt.Errorf("sample type %q/nanoseconds not found", kind)
	}
	// Normalize before grouping, including for the optional standard pprof export.
	for _, f := range p.Function {
		if f.Name == "" {
			f.Name = f.SystemName
		}
	}

	out := &summary{SampleType: kind, SampleUnit: "nanoseconds", SampleIndex: index, Samples: len(p.Sample), DurationNS: p.DurationNanos, Flat: []functionTotal{}, Cumulative: []functionTotal{}, LabelGroups: []labelTotal{}}
	totals := map[identity]*functionTotal{}
	labels := map[string]*labelTotal{}

	for _, sample := range p.Sample {
		value := sample.Value[index]
		if value < 0 || out.TotalNS > math.MaxInt64-value {
			return nil, errors.New("negative CPU sample or nanosecond total overflow")
		}

		out.TotalNS += value
		group := labelTotal{Labels: sample.Label, NumLabels: sample.NumLabel, NumUnits: sample.NumUnit}

		encoded, err := json.Marshal(group)
		if err != nil {
			return nil, err
		}

		key := string(encoded)
		if labels[key] == nil {
			labels[key] = &group
		}

		labels[key].TotalNS += value
		labels[key].Samples++
		seen := map[identity]bool{}
		leaf := true

		for _, loc := range sample.Location {
			lines := loc.Line
			if len(lines) == 0 {
				lines = []profile.Line{{}}
			}
			// Both locations and inline lines are innermost first in pprof.
			for _, line := range lines {
				entry := describe(loc, line.Function)

				id := entry.identity
				if totals[id] == nil {
					totals[id] = &entry
				}

				if leaf {
					totals[id].FlatNS += value
					leaf = false
				}

				if !seen[id] {
					totals[id].CumulativeNS += value
					seen[id] = true
				}
			}
		}

		if leaf {
			out.UnattributedNS += value
		}
	}

	for _, entry := range totals {
		out.Flat = append(out.Flat, *entry)
		out.Cumulative = append(out.Cumulative, *entry)
	}

	sort.Slice(out.Flat, func(i, j int) bool {
		a, b := out.Flat[i], out.Flat[j]
		if a.FlatNS != b.FlatNS {
			return a.FlatNS > b.FlatNS
		}

		return lessIdentity(a.identity, b.identity)
	})
	sort.Slice(out.Cumulative, func(i, j int) bool {
		a, b := out.Cumulative[i], out.Cumulative[j]
		if a.CumulativeNS != b.CumulativeNS {
			return a.CumulativeNS > b.CumulativeNS
		}

		return lessIdentity(a.identity, b.identity)
	})

	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		out.LabelGroups = append(out.LabelGroups, *labels[key])
	}

	return out, nil
}

func lessIdentity(a, b identity) bool {
	if a.MappingID != b.MappingID {
		return a.MappingID < b.MappingID
	}

	if a.FunctionID != b.FunctionID {
		return a.FunctionID < b.FunctionID
	}

	return a.LocationID < b.LocationID
}

func describe(loc *profile.Location, f *profile.Function) functionTotal {
	entry := functionTotal{}
	if loc.Mapping != nil {
		entry.MappingID = loc.Mapping.ID
		entry.Module = loc.Mapping.File
		entry.BuildID = loc.Mapping.BuildID
	}

	if f != nil {
		entry.FunctionID = f.ID
		entry.Name = f.Name
		entry.SystemName = f.SystemName

		entry.Filename = f.Filename
		if entry.Name == "" {
			entry.Name = fmt.Sprintf("[function:%d]", f.ID)
		}
	} else {
		entry.LocationID = loc.ID
		entry.Name = fmt.Sprintf("[location:%d]", loc.ID)
	}

	return entry
}
