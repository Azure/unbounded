// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (i *Installer) disablePXE(ctx context.Context, serveURL string) error {
	url := strings.TrimRight(serveURL, "/") + "/pxe/disable"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer closeBestEffort(resp.Body)

	io.Copy(io.Discard, resp.Body) //nolint:errcheck // Drain best-effort before closing.

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("GET %s returned %s", url, resp.Status)
	}

	return nil
}
