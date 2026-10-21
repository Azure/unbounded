// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"errors"
	"strings"
)

// ParseAzureContainerURL splits an Azure Blob container URL into the account
// URL and the container name. The last path segment is the container; what
// precedes it must be a valid account URL (see ValidateAzureAccountURL), so
// Azurite-style URLs such as http://127.0.0.1:10000/account/container work.
// credential is "default" or "anonymous"; the default credential requires HTTPS.
func ParseAzureContainerURL(value, credential string) (accountURL, container string, err error) {
	invalid := errors.New("container URL must be an HTTP(S) Azure Blob container URL without credentials, query, fragment, or trailing slash")

	if strings.ContainsAny(value, "?#") {
		return "", "", invalid
	}

	scheme, rest, ok := strings.Cut(value, "://")
	if !ok {
		return "", "", invalid
	}

	slash := strings.LastIndexByte(rest, '/')
	if slash < 1 || rest[slash-1] == '/' {
		return "", "", invalid
	}

	accountURL, container = scheme+"://"+rest[:slash], rest[slash+1:]
	if !ValidAzureContainerName(container) {
		return "", "", errors.New("container URL must end in a valid Azure container name")
	}

	if err := ValidateAzureAccountURL(accountURL, credential); err != nil {
		return "", "", err
	}

	return accountURL, container, nil
}

// ValidAzureContainerName reports whether name follows the Azure Blob
// container naming rules: 3 to 63 lowercase letters, digits, and single
// hyphens, starting and ending with a letter or digit.
func ValidAzureContainerName(name string) bool {
	if len(name) < 3 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' || strings.Contains(name, "--") {
		return false
	}

	return strings.IndexFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
	}) == -1
}
