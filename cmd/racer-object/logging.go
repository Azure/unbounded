// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go/logging"
)

const (
	logLevelInfo  = "info"
	logLevelDebug = "debug"

	// Each event and category may log diagnosticBurst warn lines per
	// diagnosticWindow; the next line after the window reports the rest.
	diagnosticBurst  = 10
	diagnosticWindow = time.Minute
)

// diagnosticDebug is set once from --log-level before serving starts.
var diagnosticDebug atomic.Bool

// expectedCategories are outcomes caused by the client or by normal object
// churn, not by a fault in racer-object or its upstream.
var expectedCategories = map[string]bool{
	"canceled": true, "notfound": true, "versionmismatch": true, "forbidden": true,
	"unauthorized": true, "invalid": true, "range": true,
}

var diagnosticLimiter = newLogLimiter(diagnosticBurst, diagnosticWindow, time.Now)

// logOutcome logs a request outcome. Expected categories are logged only at
// debug level. Other categories are rate limited per event and category.
// line must contain only fixed or validated fields.
func logOutcome(event, category, line string) {
	if expectedCategories[category] {
		if diagnosticDebug.Load() {
			log.Print(line + " level=debug")
		}

		return
	}

	allowed, suppressed := diagnosticLimiter.allow(event + "/" + category)
	if !allowed {
		return
	}

	if suppressed > 0 {
		line += fmt.Sprintf(" suppressed=%d", suppressed)
	}

	log.Print(line + " level=warn")
}

type logLimiter struct {
	mu     sync.Mutex
	burst  int
	window time.Duration
	now    func() time.Time
	keys   map[string]*logLimit
}

type logLimit struct {
	start      time.Time
	count      int
	suppressed int
}

func newLogLimiter(burst int, window time.Duration, now func() time.Time) *logLimiter {
	return &logLimiter{burst: burst, window: window, now: now, keys: map[string]*logLimit{}}
}

// allow reports whether to log, and how many lines were dropped for the key
// since the last allowed line. Keys are fixed categories, so the map is bounded.
func (l *logLimiter) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	limit := l.keys[key]
	if limit == nil || now.Sub(limit.start) >= l.window {
		suppressed := 0
		if limit != nil {
			suppressed = limit.suppressed
		}

		l.keys[key] = &logLimit{start: now, count: 1}

		return true, suppressed
	}

	if limit.count >= l.burst {
		limit.suppressed++

		return false, 0
	}

	limit.count++

	return true, 0
}

// awsLogger records that the SDK emitted a warning, or a debug line when
// debug is set, on the process log (stderr). It writes only fixed fields: the
// SDK's format string and arguments are never printed, because they can carry
// upstream error text, object keys, and URLs (for example, the retry
// middleware includes the original error in its messages).
func awsLogger(debug bool) logging.Logger {
	return logging.LoggerFunc(func(classification logging.Classification, _ string, _ ...any) {
		level := "warn"

		switch classification {
		case logging.Warn:
		case logging.Debug:
			if !debug {
				return
			}

			level = "debug"
		default:
			return
		}

		log.Print("racer-object: mode=origin event=aws_sdk level=" + level)
	})
}

// awsLogMode enables only retry and deprecation logs in debug mode. awsLogger
// reduces them to fixed fields, so they show only that the SDK retried or
// warned, not why. Request
// and response logging is never enabled: it prints headers (including the
// SigV4 Authorization header), bodies, and signing details.
func awsLogMode(debug bool) aws.ClientLogMode {
	if !debug {
		return 0
	}

	return aws.LogRetries | aws.LogDeprecatedUsage
}
