// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// bmcSession holds an authenticated Redfish session or basic-auth fallback
// for a single BMC.
type bmcSession struct {
	httpClient *http.Client
	baseURL    string
	token      string // X-Auth-Token; empty means basic-auth fallback
	user       string
	pass       string
	location   string // session URI for DELETE on close
}

func createSession(ctx context.Context, httpClient *http.Client, baseURL, user, pass string) (token, location string, err error) {
	url := strings.TrimRight(baseURL, "/") + "/redfish/v1/SessionService/Sessions"

	payload, err := json.Marshal(map[string]string{
		"UserName": user,
		"Password": pass,
	})
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}

	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("redfish %s %s failed: %w", http.MethodPost, "/redfish/v1/SessionService/Sessions", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // Best-effort read of error body.
		return "", "", redfishResponseError(http.MethodPost, "/redfish/v1/SessionService/Sessions", resp.StatusCode, body, nil)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	if err := checkCompletion(data); err != nil {
		return "", "", redfishResponseError(http.MethodPost, "/redfish/v1/SessionService/Sessions", resp.StatusCode, data, err)
	}

	token = resp.Header.Get("X-Auth-Token")
	if token == "" {
		return "", "", fmt.Errorf("no X-Auth-Token in session response")
	}

	location = resp.Header.Get("Location")
	if location != "" && !strings.HasPrefix(location, "http") {
		location = strings.TrimRight(baseURL, "/") + location
	}

	return token, location, nil
}

func (s *bmcSession) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	resp, err := s.send(ctx, method, path, body, nil)

	return resp.data, resp.status, err
}

type sessionResponse struct {
	data   []byte
	status int
	header http.Header
}

func (s *bmcSession) send(ctx context.Context, method, path string, body any, header http.Header) (sessionResponse, error) {
	resp, err := s.sendOnce(ctx, method, path, body, header)
	if err != nil {
		return resp, err
	}

	// If the session token expired, re-authenticate and retry once.
	if resp.status == http.StatusUnauthorized && s.token != "" {
		token, location, authErr := createSession(ctx, s.httpClient, s.baseURL, s.user, s.pass)
		if authErr != nil {
			return resp, nil
		}

		s.token = token
		s.location = location

		return s.sendOnce(ctx, method, path, body, header)
	}

	return resp, nil
}

func (s *bmcSession) sendOnce(ctx context.Context, method, path string, body any, header http.Header) (sessionResponse, error) {
	url := strings.TrimRight(s.baseURL, "/") + path

	var bodyReader io.Reader

	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return sessionResponse{}, err
		}

		bodyReader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return sessionResponse{}, err
	}

	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if s.token != "" {
		req.Header.Set("X-Auth-Token", s.token)
	} else if s.user != "" {
		req.SetBasicAuth(s.user, s.pass)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return sessionResponse{}, fmt.Errorf("redfish %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	result := sessionResponse{status: resp.StatusCode, header: resp.Header}

	result.data, err = io.ReadAll(resp.Body)
	if err != nil {
		return result, fmt.Errorf("reading Redfish %s %s response body: %w", method, path, err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := checkCompletion(result.data); err != nil {
			return result, redfishResponseError(method, path, resp.StatusCode, result.data, err)
		}
	}

	return result, nil
}

// close removes the session from the BMC (best-effort).
func (s *bmcSession) close() {
	if s.location == "" || s.token == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.location, nil)
	if err != nil {
		return
	}

	req.Header.Set("X-Auth-Token", s.token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return
	}

	resp.Body.Close() //nolint:errcheck // Best-effort close of session delete response.

	slog.Info("Redfish deleted session", "url", s.baseURL)
}
