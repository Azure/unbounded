package redfish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	location   string // session URI for DELETE
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

func (s *bmcSession) get(ctx context.Context, path string) ([]byte, int, error) {
	url := strings.TrimRight(s.baseURL, "/") + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	if s.token != "" {
		req.Header.Set("X-Auth-Token", s.token)
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

func (s *bmcSession) post(ctx context.Context, path string, body any) ([]byte, int, error) {
	url := strings.TrimRight(s.baseURL, "/") + path

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	if s.token != "" {
		req.Header.Set("X-Auth-Token", s.token)
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

func (s *bmcSession) patch(ctx context.Context, path string, body any) ([]byte, int, error) {
	url := strings.TrimRight(s.baseURL, "/") + path

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	if s.token != "" {
		req.Header.Set("X-Auth-Token", s.token)
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

// delete removes the session from the BMC (best-effort).
func (s *bmcSession) delete() {
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
}
