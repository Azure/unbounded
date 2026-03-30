package redfish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// bmcSession holds an authenticated Redfish session or basic-auth fallback
// for a single BMC.
type bmcSession struct {
	httpClient *http.Client
	baseURL    string
	user       string
	pass       string

	mu       sync.Mutex
	token    string // X-Auth-Token; empty means basic-auth fallback
	location string // session URI for DELETE
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
		return "", "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // Best-effort read of error body.
		return "", "", fmt.Errorf("session creation returned %d: %s", resp.StatusCode, body)
	}

	io.Copy(io.Discard, resp.Body) //nolint:errcheck // Best-effort drain of response body.

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

// reauth creates a new Redfish session, replacing an expired token.
func (s *bmcSession) reauth(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, location, err := createSession(ctx, s.httpClient, s.baseURL, s.user, s.pass)
	if err != nil {
		return err
	}

	s.token = token
	s.location = location
	return nil
}

// doRequest sends an HTTP request with session or basic-auth credentials
// and returns the raw response body, status code, and any transport error.
func (s *bmcSession) doRequest(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	url := strings.TrimRight(s.baseURL, "/") + path

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, 0, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	s.mu.Lock()
	token := s.token
	s.mu.Unlock()

	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	} else if s.user != "" {
		req.SetBasicAuth(s.user, s.pass)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return data, resp.StatusCode, nil
}

// tokenSet reports whether the session has an auth token (vs basic-auth fallback).
func (s *bmcSession) tokenSet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token != ""
}

func (s *bmcSession) get(ctx context.Context, path string) ([]byte, int, error) {
	data, status, err := s.doRequest(ctx, http.MethodGet, path, nil)
	if err == nil && status == http.StatusUnauthorized && s.tokenSet() {
		if s.reauth(ctx) == nil {
			return s.doRequest(ctx, http.MethodGet, path, nil)
		}
	}
	return data, status, err
}

func (s *bmcSession) post(ctx context.Context, path string, body any) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}

	data, status, err := s.doRequest(ctx, http.MethodPost, path, payload)
	if err == nil && status == http.StatusUnauthorized && s.tokenSet() {
		if s.reauth(ctx) == nil {
			return s.doRequest(ctx, http.MethodPost, path, payload)
		}
	}
	return data, status, err
}

func (s *bmcSession) patch(ctx context.Context, path string, body any) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}

	data, status, err := s.doRequest(ctx, http.MethodPatch, path, payload)
	if err == nil && status == http.StatusUnauthorized && s.tokenSet() {
		if s.reauth(ctx) == nil {
			return s.doRequest(ctx, http.MethodPatch, path, payload)
		}
	}
	return data, status, err
}

// delete removes the session from the BMC (best-effort).
func (s *bmcSession) delete() {
	s.mu.Lock()
	location := s.location
	token := s.token
	s.mu.Unlock()

	if location == "" || token == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, location, nil)
	if err != nil {
		return
	}

	req.Header.Set("X-Auth-Token", token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return
	}

	resp.Body.Close() //nolint:errcheck // Best-effort close of session delete response.
}
