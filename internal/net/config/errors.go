// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import "errors"

var (
	// ErrNoCIDRsConfigured is returned when neither IPv4 nor IPv6 CIDRs are configured.
	ErrNoCIDRsConfigured = errors.New("at least one of --ipv4-cidrs or --ipv6-cidrs must be specified")
	// ErrIPv4MaskSizeRequired is returned when IPv4 CIDRs are configured but no mask size is specified.
	ErrIPv4MaskSizeRequired = errors.New("--ipv4-mask-size is required when --ipv4-cidrs is specified")
	// ErrIPv6MaskSizeRequired is returned when IPv6 CIDRs are configured but no mask size is specified.
	ErrIPv6MaskSizeRequired = errors.New("--ipv6-mask-size is required when --ipv6-cidrs is specified")
	// ErrInvalidIPv6CIDR is returned when the first IPv6 CIDR cannot be parsed.
	ErrInvalidIPv6CIDR = errors.New("invalid IPv6 CIDR format")
)
