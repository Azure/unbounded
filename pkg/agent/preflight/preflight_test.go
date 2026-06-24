// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package preflight

import (
	"context"
	"testing"
	"time"

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

func TestRunPreservesInputOrderWhileRunningConcurrently(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 2)

	report := make(chan Report, 1)

	go func() {
		report <- Run(context.Background(), []Checker{
			blockingChecker{name: "first", started: started, release: release},
			blockingChecker{name: "second", started: started, release: release},
		}, Options{})
	}()

	seen := map[string]bool{}

	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("checks did not start concurrently")
		}
	}

	assert.True(t, seen["first"])
	assert.True(t, seen["second"])

	close(release)

	got := <-report
	assert.Equal(t, "first", got.Checks[0].Name)
	assert.Equal(t, "second", got.Checks[1].Name)
}

type blockingChecker struct {
	name    string
	started chan<- string
	release <-chan struct{}
}

func (b blockingChecker) Name() string { return b.name }

func (b blockingChecker) Check(context.Context) []Result {
	b.started <- b.name

	<-b.release

	return ResultsOK(b.name, b.name, b.name)
}
