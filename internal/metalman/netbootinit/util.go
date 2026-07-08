// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"io"
	"os"
	"time"
)

func runBestEffort(ctx context.Context, runner CommandRunner, name string, args ...string) {
	if err := runner.Run(ctx, name, args...); err != nil {
		return
	}
}

func closeBestEffort(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}

func retry(ctx context.Context, attempts int, delay time.Duration, desc string, sleep func(time.Duration), log *Logger, fn func() error) error {
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := fn(); err == nil {
			if attempt > 1 {
				log.Printf("%s succeeded (attempt %d/%d)", desc, attempt, attempts)
			}

			return nil
		} else {
			lastErr = err
		}

		if attempt == attempts {
			return lastErr
		}

		log.Printf("%s failed (attempt %d/%d), retrying in %ds", desc, attempt, attempts, int(delay.Seconds()))

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			sleep(delay)
		}
	}

	return lastErr
}

func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return string(data)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
