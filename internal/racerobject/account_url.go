// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ValidateAzureAccountURL checks an Azure Blob account URL for the origin.
// credential is "default" or "anonymous"; the default credential requires HTTPS.
// Azurite-style URLs may carry one simple account path segment.
func ValidateAzureAccountURL(value, credential string) error {
	if credential != "default" && credential != "anonymous" {
		return errors.New("--azure-credential must be default or anonymous")
	}

	invalid := errors.New("--account-url must be an HTTP(S) account URL without credentials, query, fragment, or more than one simple path segment")

	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") ||
		strings.Contains(u.Host, "%") || strings.HasSuffix(u.Host, ":") {
		return invalid
	}

	if strings.Contains(u.Hostname(), ":") || strings.HasPrefix(u.Host, "[") {
		if !strings.HasPrefix(u.Host, "[") || net.ParseIP(u.Hostname()) == nil {
			return invalid
		}
	}

	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid --account-url port")
		}
	}
	// Only plain account names are allowed in an Azurite path prefix.
	segment := strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if strings.IndexFunc(segment, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) != -1 || u.EscapedPath() == "//" {
		return invalid
	}

	if credential == "default" && u.Scheme != "https" {
		return errors.New("--account-url requires HTTPS with --azure-credential=default")
	}

	return nil
}

// AzureOriginIdentity returns the stable upstream identity for an Azure account URL.
// It lowercases the scheme and host, drops the default port, and trims one trailing
// slash so that equivalent spellings of the same account share cache entries.
// The URL must already pass ValidateAzureAccountURL.
func AzureOriginIdentity(accountURL string) (string, error) {
	u, err := url.Parse(accountURL)
	if err != nil {
		return "", errors.New("invalid account URL")
	}

	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)

	if port := u.Port(); (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		host = strings.TrimSuffix(host, ":"+port)
	}

	path := strings.TrimSuffix(u.EscapedPath(), "/")

	return scheme + "://" + host + path, nil
}
