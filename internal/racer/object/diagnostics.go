// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import "errors"

// invalidS3Response refines errInvalidS3Response for diagnostics only. It
// still matches errInvalidS3Response, so the Racer and sidecar mapping (502)
// is unchanged.
type invalidS3Response struct{ kind string }

// Error matches errInvalidS3Response so messages stay unchanged.
func (e *invalidS3Response) Error() string { return errInvalidS3Response.Error() }

func (e *invalidS3Response) Is(target error) bool { return target == errInvalidS3Response }

var (
	// errS3Redirect is a 3xx, typically a wrong region or addressing style.
	errS3Redirect error = &invalidS3Response{kind: "redirect"}
	// errS3Status is an HTTP status with no Racer meaning, such as 400 or 405.
	errS3Status error = &invalidS3Response{kind: "status"}
	// errS3Validation is a response that failed HEAD or GET validation.
	errS3Validation error = &invalidS3Response{kind: "validation"}
)

func invalidS3Status(code int) error {
	if code >= 300 && code < 400 {
		return errS3Redirect
	}

	return errS3Status
}

// InvalidResponseCategory returns a fixed diagnostic category for an invalid S3
// response: "redirect", "status", "validation", or "upstream" when the cause is
// unknown. It returns "" when err is not an invalid S3 response. The result
// never contains upstream text.
func InvalidResponseCategory(err error) string {
	var invalid *invalidS3Response
	if errors.As(err, &invalid) {
		return invalid.kind
	}

	if errors.Is(err, errInvalidS3Response) {
		return "upstream"
	}

	return ""
}
