// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"strconv"
	"strings"
)

// parseMaxAge matches the client's HEAD-only retention grammar for the fake peer.
func parseMaxAge(values []string) (uint64, error) {
	var seconds uint64

	seen := false
	bad := failure(ErrorInvalidArgument, "max-age", nil)

	for _, value := range values {
		quoted, escaped, start := false, false, 0

		for i := 0; i <= len(value); i++ {
			b := byte(',')
			if i < len(value) {
				b = value[i]
			}

			switch {
			case escaped:
				escaped = false
			case quoted && b == '\\':
				escaped = true
			case b == '"':
				quoted = !quoted
			case b == ',' && !quoted:
				directive := strings.Trim(value[start:i], " \t")
				start = i + 1

				end := 0
				for end < len(directive) && headerToken(directive[end]) {
					end++
				}

				if !strings.EqualFold(directive[:end], "max-age") {
					continue
				}

				argument, ok := strings.CutPrefix(strings.Trim(directive[end:], " \t"), "=")
				if seen || !ok {
					return 0, bad
				}

				seen = true

				digits := strings.Trim(argument, " \t")
				if strings.HasPrefix(digits, "\"") && strings.HasSuffix(digits, "\"") && len(digits) >= 2 {
					digits = digits[1 : len(digits)-1]
				}

				if digits == "" || strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
					return 0, bad
				}

				var err error

				seconds, err = strconv.ParseUint(digits, 10, 64)
				if err != nil {
					return 0, bad
				}
			}
		}

		if quoted || escaped {
			return 0, bad
		}
	}

	return seconds, nil
}
