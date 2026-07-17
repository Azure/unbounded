// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func runURLCheck(args []string, output io.Writer, hold bool) error {
	if len(args) < 2 || len(args) > 3 {
		return fmt.Errorf("usage: acr-origin-proxy <get-url|check-url|probe-health> <url> <timeout> [accept]")
	}

	target, err := url.ParseRequestURI(args[0])
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return fmt.Errorf("invalid probe URL %q", args[0])
	}

	timeout, err := time.ParseDuration(args[1])
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid probe timeout %q", args[1])
	}

	accept := ""
	if len(args) == 3 {
		accept = args[2]
	}

	if err := fetchURL(context.Background(), &http.Client{Timeout: timeout}, target.String(), accept, output); err != nil {
		return err
	}

	if !hold {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	return nil
}

func fetchURL(ctx context.Context, client *http.Client, target, accept string, output io.Writer) (returnErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}

	if accept != "" {
		request.Header.Set("Accept", accept)
	}

	response, err := client.Do(request)
	if err != nil {
		return err
	}

	defer func() {
		if err := response.Body.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("close probe response: %w", err)
		}
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024)) //nolint:errcheck // The HTTP status is the authoritative probe failure.

		return fmt.Errorf("probe returned HTTP %d", response.StatusCode)
	}

	if _, err := io.Copy(output, response.Body); err != nil {
		return fmt.Errorf("read probe response: %w", err)
	}

	return nil
}
