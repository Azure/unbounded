// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Azure/unbounded/internal/racer/workload"
)

// dataplaneSettingsJSON is generated from the dataplane's configuration table
// (cmd/racer/src/config/table.rs). Regenerate it with
// `BLESS=1 cargo test -p racer config::tests::generated_reference_is_current`
// in cmd/racer; that test fails when this file is stale.
//
//go:embed dataplane-settings.json
var dataplaneSettingsJSON []byte

// dataplaneSetting is one entry of the generated table.
type dataplaneSetting struct {
	Name    string   `json:"name"`
	Owner   string   `json:"owner"`
	Scope   string   `json:"scope"`
	Kind    string   `json:"kind"`
	Min     string   `json:"min"`
	Max     string   `json:"max"`
	Step    string   `json:"step"`
	Values  []string `json:"values"`
	Default *string  `json:"default"`
	// EmptyUnset marks settings where "" means unset, as Kubernetes expands
	// absent field references.
	EmptyUnset bool `json:"empty_unset"`
}

// dataplaneSettings indexes the generated table by name. The checks below
// apply the same per-key rules the dataplane applies at startup, so a value
// it would refuse fails the operator plan instead of rolling Pods into a
// crash loop. Cross-key rules that need the node, such as plaintext budgets
// that must fit the range window, are left to the dataplane.
var dataplaneSettings = func() map[string]dataplaneSetting {
	var list []dataplaneSetting

	if err := json.Unmarshal(dataplaneSettingsJSON, &list); err != nil {
		panic(fmt.Sprintf("parse dataplane-settings.json: %v", err))
	}

	settings := make(map[string]dataplaneSetting, len(list))
	for _, setting := range list {
		settings[setting.Name] = setting
	}

	return settings
}()

// maxDataplaneValueBytes mirrors MAX_VALUE_BYTES in cmd/racer/src/config/spec.rs.
const maxDataplaneValueBytes = 4096

// checkDataplaneValue applies one setting's syntax and bounds, returning the
// rule it breaks or "" when the value is accepted.
func checkDataplaneValue(setting dataplaneSetting, value string) string {
	if value == "" {
		if setting.EmptyUnset {
			return ""
		}

		return "must not be empty"
	}

	if len(value) > maxDataplaneValueBytes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Sprintf("must be at most %d bytes without control characters", maxDataplaneValueBytes)
	}

	switch setting.Kind {
	case "bool":
		if value != "true" && value != "false" {
			return "must be true or false"
		}
	case "enum":
		if !slices.Contains(setting.Values, value) {
			return "must be one of " + strings.Join(setting.Values, ", ")
		}
	case "unsigned":
		minimum, maximum, step, err := unsignedBounds(setting)
		if err != nil {
			return "cannot be checked: " + err.Error()
		}

		n, valid := dataplaneDecimal(value)
		if !valid || n < minimum || n > maximum {
			return fmt.Sprintf("must be an integer from %d to %d", minimum, maximum)
		}

		if step > 1 && n%step != 0 {
			return fmt.Sprintf("must be a multiple of %d", step)
		}
	}

	return ""
}

// unsignedBounds parses an unsigned setting's bounds from the generated table.
// An empty bound means none: Min defaults to 0, Max to the uint64 maximum, and
// Step to 1.
func unsignedBounds(setting dataplaneSetting) (minimum, maximum, step uint64, err error) {
	parse := func(field, raw string, fallback uint64) (uint64, error) {
		if raw == "" {
			return fallback, nil
		}

		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("setting %s has invalid %s bound %q: %w", setting.Name, field, raw, err)
		}

		return n, nil
	}

	if minimum, err = parse("min", setting.Min, 0); err != nil {
		return 0, 0, 0, err
	}

	if maximum, err = parse("max", setting.Max, math.MaxUint64); err != nil {
		return 0, 0, 0, err
	}

	if step, err = parse("step", setting.Step, 1); err != nil {
		return 0, 0, 0, err
	}

	return minimum, maximum, step, nil
}

// validateDataplaneConfig reports every racer-settings value the
// dataplane or operator would reject, sorted by key, in one error.
func validateDataplaneConfig(data map[string]string) error {
	var problems []string

	add := func(key, rule string) {
		problems = append(problems, key+" "+rule)
	}

	for key, value := range data {
		if !strings.HasPrefix(key, "RACER_") {
			continue
		}

		setting, known := dataplaneSettings[key]
		if !known {
			if retired, ok := retiredSettings[key]; ok {
				add(key, "is retired; set "+retired.replacement+" instead (check its units and default before copying the value)")

				continue
			}

			add(key, "is not a recognized setting; unknown RACER_* variables fail dataplane startup")

			continue
		}

		if rule := checkDataplaneValue(setting, value); rule != "" {
			add(key, rule)
		}

		if _, both := data[key+"_PER_WORKER"]; both {
			if _, paired := dataplaneSettings[key+"_PER_WORKER"]; paired {
				add(key, "and "+key+"_PER_WORKER must not both be set; the first is a node-wide total, the second a per-worker amount")
			}
		}
	}

	_, ioSet := data["RACER_IO_WORKERS"]
	if _, helpersSet := data["RACER_CRYPTO_HELPERS"]; ioSet != helpersSet {
		add("RACER_IO_WORKERS", "and RACER_CRYPTO_HELPERS must be set together")
	}

	if value, ok := data[workload.ControlSeatsKey]; ok && value != "auto" {
		// The runtime accepts only the canonical decimal form, so reject
		// leading zeros, signs, and whitespace before Pods roll.
		seats, err := strconv.ParseUint(value, 10, 8)
		if err != nil || strconv.FormatUint(seats, 10) != value || seats < 1 || seats > workload.MaxControlSeats {
			add(workload.ControlSeatsKey, fmt.Sprintf("must be auto or an integer from 1 to %d", workload.MaxControlSeats))
		}
	}

	if value, ok := data["RACER_MAX_THREADS"]; ok {
		if threads, valid := dataplaneDecimal(value); !valid || threads < minThreads {
			add("RACER_MAX_THREADS", fmt.Sprintf("must be an integer of at least %d, including the control plane thread", minThreads))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	sort.Strings(problems)

	return fmt.Errorf("ConfigMap %s is invalid, so the dataplane DaemonSet keeps its current configuration: %s", dataplaneConfigName, strings.Join(problems, "; "))
}

// dataplaneDecimal mirrors the dataplane's text and decimal rules: nonempty,
// at most 4096 bytes, no control characters, ASCII digits only, and within
// the u64 range.
func dataplaneDecimal(value string) (uint64, bool) {
	if value == "" || len(value) > 4096 || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' || unicode.IsControl(r) }) >= 0 {
		return 0, false
	}

	trimmed := strings.TrimLeft(value, "0")
	if trimmed == "" {
		return 0, true
	}

	n, err := strconv.ParseUint(trimmed, 10, 64)

	return n, err == nil
}

// retiredSetting describes a RACER_* key that an earlier release accepted
// and the current dataplane rejects at startup.
type retiredSetting struct {
	// replacement names the key that took over, for error messages.
	replacement string

	// shippedDefault is the value the earlier racer-settings template wrote,
	// or empty when the template never wrote the key.
	shippedDefault string
}

// retiredSettings lists retired keys an administrator or an earlier template
// may have left in racer-settings (see the rename table in cmd/racer/README.md).
//
// The budget keys are not renamed automatically: the old keys were node-wide
// totals, while their replacements are per-worker amounts multiplied by the
// worker count, so copying a value would silently change its meaning. Only a
// value equal to what the earlier template shipped is migrated, by dropping
// it so the new default applies; that default replaced the old one. Any other
// value fails validation with a message naming the replacement, so the
// administrator chooses the new value.
var retiredSettings = map[string]retiredSetting{
	"RACER_SLAB_BYTES":                   {replacement: "RACER_SLAB_BYTES_PER_WORKER", shippedDefault: "1073741824"},
	"RACER_FREE_SEGMENT_RESERVE":         {replacement: "RACER_FREE_SEGMENT_RESERVE_PER_WORKER", shippedDefault: "2"},
	"RACER_ACTIVE_PATH_SEARCHES":         {replacement: "RACER_ACTIVE_PATH_SEARCHES_PER_WORKER"},
	"RACER_CONNECTIONS_PER_NEIGHBOR":     {replacement: "RACER_NEIGHBOR_CONNECTIONS_PER_WORKER"},
	"RACER_ORIGIN_CONNECTIONS_PER_CACHE": {replacement: "RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER"},
	"RACER_SEND_CRC_PAIR":                {replacement: "RACER_SEND_TAG_PAIR"},
}

// migrateRetiredSettings returns data without retired keys that still hold
// the earlier template's default, and whether it removed any. The input is
// not modified. Other retired keys are kept so validation reports them.
func migrateRetiredSettings(data map[string]string) (map[string]string, bool) {
	changed := false

	for key, retired := range retiredSettings {
		if value, ok := data[key]; ok && retired.shippedDefault != "" && value == retired.shippedDefault {
			changed = true
		}
	}

	if !changed {
		return data, false
	}

	out := make(map[string]string, len(data))

	for key, value := range data {
		if retired, ok := retiredSettings[key]; ok && retired.shippedDefault != "" && value == retired.shippedDefault {
			continue
		}

		out[key] = value
	}

	return out, true
}
