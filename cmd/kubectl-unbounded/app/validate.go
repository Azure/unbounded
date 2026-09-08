// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"net"
	"os"
	"strings"
)

func isValidIPv4CIDR(s string) bool {
	ip, _, err := net.ParseCIDR(s)
	if err != nil {
		return false
	}

	return ip.To4() != nil
}

func isReadableFile(p string) bool {
	if isEmpty(p) {
		return false
	}

	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}

	f, err := os.Open(p)
	if err != nil {
		return false
	}

	return f.Close() == nil
}

func isEmpty(s string) bool {
	return strings.TrimSpace(s) == ""
}
