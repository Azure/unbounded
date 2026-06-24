// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package preflight

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

type fakeChecker struct {
	name    string
	results []Result
}

func (f fakeChecker) Name() string { return f.name }

func (f fakeChecker) Check(context.Context) []Result { return f.results }

func TestRunIncludesAllResults(t *testing.T) {
	report := Run(context.Background(), []Checker{
		fakeChecker{results: []Result{OK("agent-config", "agent config", "valid")}},
		fakeChecker{results: []Result{Warning("swap-active", "host swap", "enabled")}},
		fakeChecker{results: []Result{Error("host-packages", "host packages", "missing")}},
	}, Options{})

	assert.Equal(t, "failed", report.Status)
	assert.Equal(t, Summary{OK: 1, Warnings: 1, Errors: 1}, report.Summary)
	assert.Len(t, report.Checks, 3)
}

func TestRunDowngradesIgnoredErrors(t *testing.T) {
	report := Run(context.Background(), []Checker{
		fakeChecker{results: []Result{Error("host-packages", "host packages", "missing")}},
	}, Options{IgnoreErrors: []string{"host-packages"}})

	assert.Equal(t, "ok", report.Status)
	assert.Equal(t, Summary{Warnings: 1}, report.Summary)
	assert.True(t, report.Checks[0].Ignored)
	assert.Equal(t, SeverityWarning, report.Checks[0].Severity)
}

func TestRunFailOnWarnings(t *testing.T) {
	report := Run(context.Background(), []Checker{
		fakeChecker{results: []Result{Warning("swap-active", "host swap", "enabled")}},
	}, Options{FailOnWarnings: true})

	assert.Equal(t, "failed", report.Status)
	assert.Error(t, report.Err(true))
}

func TestRunIgnoreAll(t *testing.T) {
	report := Run(context.Background(), []Checker{
		fakeChecker{results: []Result{Error("host-packages", "host packages", "missing")}},
	}, Options{IgnoreErrors: []string{"all"}})

	assert.Equal(t, "ok", report.Status)
	assert.True(t, report.Checks[0].Ignored)
}
