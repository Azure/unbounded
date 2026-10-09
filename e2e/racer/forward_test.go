//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// forwardHTTP waits for the remote HTTP endpoint, not just kubectl's local
// listener. A remote connection refusal terminates kubectl, so startup retries
// must replace the process as well as retry the request. Each attempt retains
// its own log. The successful process lives until the caller invokes stop.
func forwardHTTP(ctx context.Context, command func() *exec.Cmd, logPrefix, readyPath string) (address string, stop func(), err error) {
	var (
		cmd     *exec.Cmd
		log     *os.File
		done    chan struct{}
		logPath string
		lastErr error
		attempt int
	)

	stop = func() {
		if cmd != nil {
			_ = cmd.Process.Kill()

			<-done

			_ = log.Close()
			cmd = nil
		}
	}

	defer func() {
		if err != nil {
			stop()
		}
	}()

	client := &http.Client{Timeout: time.Second}
	pattern := regexp.MustCompile(`Forwarding from (127\.0\.0\.1:\d+)`)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return "", stop, fmt.Errorf("forward readiness: %w (last attempt: %v; log: %s)", err, lastErr, logPath)
		}

		if cmd == nil {
			attempt++
			logPath = fmt.Sprintf("%s-%d.log", logPrefix, attempt)

			log, err = os.Create(logPath)
			if err != nil {
				return "", stop, err
			}

			cmd = command()

			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				_ = log.Close()
				cmd = nil

				return "", stop, err
			}

			done = make(chan struct{})
			go func(process *exec.Cmd, exited chan struct{}) {
				_ = process.Wait()

				close(exited)
			}(cmd, done)

			address = ""
		}

		select {
		case <-done:
			lastErr = fmt.Errorf("port-forward exited: %s", cmd.ProcessState)

			stop()
		default:
			if address == "" {
				data, _ := os.ReadFile(logPath)
				if match := pattern.FindSubmatch(data); len(match) == 2 {
					address = "http://" + string(match[1])
				}
			}

			if address != "" {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+readyPath, http.NoBody)
				if err != nil {
					return "", stop, err
				}

				response, err := client.Do(request)

				lastErr = err
				if err == nil {
					response.Body.Close()

					if response.StatusCode == http.StatusOK {
						return address, stop, nil
					}

					lastErr = fmt.Errorf("%s returned %s", readyPath, response.Status)
				}
			}
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
