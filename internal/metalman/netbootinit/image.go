// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
)

func (i *Installer) downloadAndWriteImage(ctx context.Context, imageURL, targetDisk string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return err
	}

	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer closeBestEffort(resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("GET %s returned %s", imageURL, resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer closeBestEffort(gz)

	out, err := os.OpenFile(targetDisk, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening target disk: %w", err)
	}
	defer closeBestEffort(out)

	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(out, gz, buf); err != nil {
		return fmt.Errorf("writing target disk: %w", err)
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing target disk: %w", err)
	}

	return nil
}
