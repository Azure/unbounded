// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package preflight provides reusable non-mutating checks for validating a host
// and agent configuration before bootstrap.
package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

type Severity string

const (
	// SeverityOK indicates the check completed successfully.
	SeverityOK Severity = "ok"
	// SeverityWarning indicates the check found a condition that bootstrap can
	// usually remediate or that does not necessarily block bootstrap.
	SeverityWarning Severity = "warning"
	// SeverityError indicates the check found a fatal condition that should block
	// bootstrap unless explicitly ignored.
	SeverityError Severity = "error"
)

// Checker is a non-mutating preflight validation unit.
type Checker interface {
	Name() string
	Check(ctx context.Context) []Result
}

// Result describes one preflight check outcome. Message and Target must not
// include raw configured values such as URLs, tokens, image references, or file
// contents.
type Result struct {
	Name     string   `json:"name"`
	Target   string   `json:"target"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Ignored  bool     `json:"ignored"`
}

// Options controls preflight result handling.
type Options struct {
	IgnoreErrors   []string
	FailOnWarnings bool
}

// Report is the complete preflight output for both text and JSON consumers.
type Report struct {
	Status  string   `json:"status"`
	Checks  []Result `json:"checks"`
	Summary Summary  `json:"summary"`
}

// Summary contains aggregate result counts after ignore handling is applied.
type Summary struct {
	OK       int `json:"ok"`
	Warnings int `json:"warnings"`
	Errors   int `json:"errors"`
}

// Run executes all checks, applies ignore rules, and returns a complete report.
func Run(ctx context.Context, checks []Checker, opts Options) Report {
	ignored := ignoreSet(opts.IgnoreErrors)
	checkResults := make([][]Result, len(checks))

	var wg sync.WaitGroup

	for i, check := range checks {
		wg.Go(func() {
			checkResults[i] = check.Check(ctx)
		})
	}

	wg.Wait()

	results := make([]Result, 0, len(checks))
	for i, check := range checks {
		for _, result := range checkResults[i] {
			if result.Name == "" {
				result.Name = check.Name()
			}

			if result.Target == "" {
				result.Target = result.Name
			}

			if result.Severity == "" {
				result.Severity = SeverityOK
			}

			if result.Severity == SeverityError && ignored(result.Name) {
				result.Severity = SeverityWarning
				result.Ignored = true
			}

			results = append(results, result)
		}
	}

	return buildReport(results, opts.FailOnWarnings)
}

// OK returns a successful check result.
func OK(name, target, message string) Result {
	return Result{Name: name, Target: target, Severity: SeverityOK, Message: message}
}

// Warning returns a warning check result. When args are provided, message is
// formatted with fmt.Sprintf.
func Warning(name, target, message string, args ...any) Result {
	return Result{Name: name, Target: target, Severity: SeverityWarning, Message: formatMessage(message, args...)}
}

// Error returns a fatal check result. When args are provided, message is
// formatted with fmt.Sprintf.
func Error(name, target, message string, args ...any) Result {
	return Result{Name: name, Target: target, Severity: SeverityError, Message: formatMessage(message, args...)}
}

// Results returns a result slice for concise checker returns and test fixtures.
func Results(results ...Result) []Result {
	return results
}

// ResultsOK returns a single successful check result as a slice.
func ResultsOK(name, target, message string) []Result {
	return Results(OK(name, target, message))
}

// ResultsWarning returns a single warning check result as a slice. When args
// are provided, message is formatted with fmt.Sprintf.
func ResultsWarning(name, target, message string, args ...any) []Result {
	return Results(Warning(name, target, message, args...))
}

// ResultsError returns a single fatal check result as a slice. When args are
// provided, message is formatted with fmt.Sprintf.
func ResultsError(name, target, message string, args ...any) []Result {
	return Results(Error(name, target, message, args...))
}

func formatMessage(message string, args ...any) string {
	if len(args) == 0 {
		return message
	}

	return fmt.Sprintf(message, args...)
}

// HasErrors reports whether any fatal errors remain after ignore handling.
func (r Report) HasErrors() bool {
	return r.Summary.Errors > 0
}

// HasWarnings reports whether the report contains any warnings.
func (r Report) HasWarnings() bool {
	return r.Summary.Warnings > 0
}

// Err converts the report status into a command error.
func (r Report) Err(failOnWarnings bool) error {
	if r.HasErrors() {
		return fmt.Errorf("preflight checks failed")
	}

	if failOnWarnings && r.HasWarnings() {
		return fmt.Errorf("preflight checks returned warnings")
	}

	return nil
}

func buildReport(results []Result, failOnWarnings bool) Report {
	summary := Summary{}

	for _, result := range results {
		switch result.Severity {
		case SeverityError:
			summary.Errors++
		case SeverityWarning:
			summary.Warnings++
		default:
			summary.OK++
		}
	}

	status := "ok"
	if summary.Errors > 0 || (failOnWarnings && summary.Warnings > 0) {
		status = "failed"
	}

	return Report{Status: status, Checks: results, Summary: summary}
}

func ignoreSet(values []string) func(string) bool {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part != "" {
				normalized = append(normalized, part)
			}
		}
	}

	return func(name string) bool {
		name = strings.ToLower(strings.TrimSpace(name))
		return slices.Contains(normalized, "all") || slices.Contains(normalized, name)
	}
}
