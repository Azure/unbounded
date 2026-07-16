// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultListenAddr        = ":5002"
	defaultMetricsListenAddr = ":9090"
	defaultAuthMode          = "auto"
	defaultMaxTokenLife      = 30 * time.Minute
	defaultRefreshSkewSecs   = 30
	maxCachedTokens          = 1024
)

type config struct {
	listen                string
	metricsListen         string
	upstream              *url.URL
	user                  string
	pass                  string
	controlToken          string
	authMode              string
	maxTokenLife          time.Duration
	refreshSkewSecs       int
	throttleBlobInflight  int
	throttleRetryAfterSec int
	runID                 string
}

type tokenEntry struct {
	token   string
	expires time.Time
}

type tokenCache struct {
	mu      sync.Mutex
	byScope map[string]tokenEntry
	skew    time.Duration
}

func newTokenCache(skewSecs int) *tokenCache {
	return &tokenCache{
		byScope: make(map[string]tokenEntry),
		skew:    time.Duration(skewSecs) * time.Second,
	}
}

func (c *tokenCache) lookup(scope string) (string, bool) {
	if scope == "" {
		return "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.byScope[scope]
	if !ok {
		return "", false
	}

	if !entry.expires.IsZero() && time.Until(entry.expires) <= c.skew {
		return "", false
	}

	return entry.token, true
}

func (c *tokenCache) store(scope, token string, expires time.Time) {
	if scope == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.byScope) >= maxCachedTokens {
		for key := range c.byScope {
			delete(c.byScope, key)

			break
		}
	}

	c.byScope[scope] = tokenEntry{token: token, expires: expires}
}

func configFromEnv() (*config, error) {
	upstream := os.Getenv("UPSTREAM_REGISTRY")
	if upstream == "" {
		return nil, errors.New("environment variable UPSTREAM_REGISTRY is required")
	}

	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse UPSTREAM_REGISTRY: %w", err)
	}

	if upstreamURL.Scheme != "https" && upstreamURL.Scheme != "http" {
		return nil, fmt.Errorf("environment variable UPSTREAM_REGISTRY scheme must be http or https, got %q", upstreamURL.Scheme)
	}

	if upstreamURL.Host == "" {
		return nil, fmt.Errorf("environment variable UPSTREAM_REGISTRY has empty host: %q", upstream)
	}

	user := os.Getenv("ACR_USERNAME")

	pass := os.Getenv("ACR_PASSWORD")
	if user == "" || pass == "" {
		return nil, errors.New("proxy ACR_USERNAME and ACR_PASSWORD are required")
	}

	runID := os.Getenv("BENCHMARK_RUN_ID")
	if runID == "" {
		return nil, errors.New("benchmark run ID is required in BENCHMARK_RUN_ID")
	}

	controlToken := os.Getenv("BENCHMARK_CONTROL_TOKEN")
	if controlToken == "" {
		return nil, errors.New("benchmark control token is required in BENCHMARK_CONTROL_TOKEN")
	}

	authMode := valueOrDefault(os.Getenv("AUTH_MODE"), defaultAuthMode)
	switch authMode {
	case "basic", "bearer", "auto":
	default:
		return nil, fmt.Errorf("authentication mode AUTH_MODE must be basic, bearer, or auto, got %q", authMode)
	}

	refreshSkewSecs, err := positiveIntEnv("REFRESH_SKEW_SECONDS", defaultRefreshSkewSecs)
	if err != nil {
		return nil, err
	}

	maxTokenLifeSecs, err := positiveIntEnv("MAX_TOKEN_LIFETIME_SECONDS", int(defaultMaxTokenLife.Seconds()))
	if err != nil {
		return nil, err
	}

	throttleBlobInflight, err := nonNegativeIntEnv("THROTTLE_BLOB_INFLIGHT", 0)
	if err != nil {
		return nil, err
	}

	throttleRetryAfterSecs, err := positiveIntEnv("THROTTLE_RETRY_AFTER_SECONDS", 5)
	if err != nil {
		return nil, err
	}

	return &config{
		listen:                valueOrDefault(os.Getenv("LISTEN_ADDR"), defaultListenAddr),
		metricsListen:         valueOrDefault(os.Getenv("METRICS_LISTEN_ADDR"), defaultMetricsListenAddr),
		upstream:              upstreamURL,
		user:                  user,
		pass:                  pass,
		controlToken:          controlToken,
		authMode:              authMode,
		maxTokenLife:          time.Duration(maxTokenLifeSecs) * time.Second,
		refreshSkewSecs:       refreshSkewSecs,
		throttleBlobInflight:  throttleBlobInflight,
		throttleRetryAfterSec: throttleRetryAfterSecs,
		runID:                 runID,
	}, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

func positiveIntEnv(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, value)
	}

	return parsed, nil
}

func nonNegativeIntEnv(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, value)
	}

	return parsed, nil
}

type bearerChallenge struct {
	realm   string
	service string
	scope   string
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	Token       string `json:"token"`
	ExpiresIn   int    `json:"expires_in"`
}

func doWithAuth(
	ctx context.Context,
	client *http.Client,
	cfg *config,
	cache *tokenCache,
	observer *observer,
	attribution phaseSnapshot,
	method string,
	target *url.URL,
	headers http.Header,
	body io.Reader,
) (*http.Response, bool, error) {
	scope := guessScope(target.Path)

	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, false, err
	}

	copyForwardedHeaders(request.Header, headers)
	attachInitialAuth(request.Header, cfg, cache, scope)

	response, err := client.Do(request)
	if err != nil {
		return nil, false, err
	}

	if response.StatusCode != http.StatusUnauthorized || cfg.authMode == "basic" {
		return response, false, nil
	}

	challenge := parseBearerChallenge(response.Header.Get("WWW-Authenticate"))
	if challenge == nil {
		return response, false, nil
	}

	_, _ = io.Copy(io.Discard, response.Body) //nolint:errcheck // Draining is best effort before the authenticated retry.
	_ = response.Body.Close()                 //nolint:errcheck // The retry does not depend on a close error from the 401 body.

	token, expires, err := exchangeToken(ctx, client, cfg, challenge)
	if err != nil {
		observer.recordAuthRefresh(attribution, "error")

		return nil, false, fmt.Errorf("token exchange (realm=%s): %w", challenge.realm, err)
	}

	observer.recordAuthRefresh(attribution, "success")

	cacheScope := challenge.scope
	if cacheScope == "" {
		cacheScope = scope
	}

	if cacheScope != "" {
		lifetimeCap := time.Now().Add(cfg.maxTokenLife)
		if expires.IsZero() || expires.After(lifetimeCap) {
			expires = lifetimeCap
		}

		cache.store(cacheScope, token, expires)
	}

	retry, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, true, err
	}

	copyForwardedHeaders(retry.Header, headers)
	retry.Header.Set("Authorization", "Bearer "+token)

	response, err = client.Do(retry)

	return response, true, err
}

func attachInitialAuth(header http.Header, cfg *config, cache *tokenCache, scope string) {
	switch cfg.authMode {
	case "basic":
		header.Set("Authorization", "Basic "+basicAuth(cfg.user, cfg.pass))
	case "bearer":
		if token, ok := cache.lookup(scope); ok {
			header.Set("Authorization", "Bearer "+token)
		}
	case "auto":
		if token, ok := cache.lookup(scope); ok {
			header.Set("Authorization", "Bearer "+token)
		} else {
			header.Set("Authorization", "Basic "+basicAuth(cfg.user, cfg.pass))
		}
	}
}

var (
	bearerSchemePattern = regexp.MustCompile(`(?i)^\s*Bearer\s+`)
	bearerParamPattern  = regexp.MustCompile(`(\w+)\s*=\s*"([^"]*)"`)
)

func parseBearerChallenge(header string) *bearerChallenge {
	if header == "" || !bearerSchemePattern.MatchString(header) {
		return nil
	}

	challenge := &bearerChallenge{}

	rest := bearerSchemePattern.ReplaceAllString(header, "")
	for _, match := range bearerParamPattern.FindAllStringSubmatch(rest, -1) {
		switch strings.ToLower(match[1]) {
		case "realm":
			challenge.realm = match[2]
		case "service":
			challenge.service = match[2]
		case "scope":
			challenge.scope = match[2]
		}
	}

	if challenge.realm == "" {
		return nil
	}

	return challenge
}

func exchangeToken(ctx context.Context, client *http.Client, cfg *config, challenge *bearerChallenge) (string, time.Time, error) {
	realmURL, err := url.Parse(challenge.realm)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse realm %q: %w", challenge.realm, err)
	}

	query := realmURL.Query()
	if challenge.service != "" {
		query.Set("service", challenge.service)
	}

	if challenge.scope != "" {
		query.Set("scope", challenge.scope)
	}

	realmURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realmURL.String(), nil)
	if err != nil {
		return "", time.Time{}, err
	}

	request.Header.Set("Authorization", "Basic "+basicAuth(cfg.user, cfg.pass))
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return "", time.Time{}, err
	}

	defer func() {
		_ = response.Body.Close() //nolint:errcheck // The token response has already been consumed.
	}()

	if response.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4*1024))
		if readErr != nil {
			return "", time.Time{}, fmt.Errorf("realm returned %d and reading its error body failed: %w", response.StatusCode, readErr)
		}

		return "", time.Time{}, fmt.Errorf("realm returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var token tokenResponse
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
		return "", time.Time{}, fmt.Errorf("decode token response: %w", err)
	}

	accessToken := token.AccessToken
	if accessToken == "" {
		accessToken = token.Token
	}

	if accessToken == "" {
		return "", time.Time{}, errors.New("token response missing access_token and token")
	}

	expires := time.Time{}
	if token.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}

	return accessToken, expires, nil
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func guessScope(path string) string {
	if !strings.HasPrefix(path, "/v2/") {
		return ""
	}

	rest := strings.TrimPrefix(path, "/v2/")

	separator, _ := rightmostOCIBoundary(rest)
	if separator <= 0 {
		return ""
	}

	return "repository:" + rest[:separator] + ":pull"
}
