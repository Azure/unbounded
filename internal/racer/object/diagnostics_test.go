// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestInvalidResponseCategory(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"redirect-301", classifyS3Error(originStatusError{301}, false), "redirect"},
		{"redirect-307", classifyS3Error(originStatusError{307}, false), "redirect"},
		{"status-405", classifyS3Error(originStatusError{405}, false), "status"},
		{"status-416", classifyS3Error(originStatusError{416}, false), "status"},
		{"validation", errS3Validation, "validation"},
		{"unknown", classifyS3Error(errors.New("private"), false), "upstream"},
		{"not-invalid", classifyS3Error(originStatusError{404}, false), ""},
		{"nil", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InvalidResponseCategory(tc.err); got != tc.want {
				t.Fatalf("category = %q, want %q", got, tc.want)
			}

			if tc.want != "" && (!errors.Is(tc.err, errInvalidS3Response) || sidecarSDKFailure(tc.err) != http.StatusBadGateway) {
				t.Fatal("sub-category changed the error mapping")
			}
		})
	}

	if errors.Is(errS3Status, racersdk.ErrUnavailable) {
		t.Fatal("status sub-category matched an unrelated sentinel")
	}
}

func TestInvalidHeadIsValidation(t *testing.T) {
	_, err := (&objectOrigin{}).headMetadata(nil, Object{})
	if InvalidResponseCategory(err) != "validation" {
		t.Fatalf("head validation category = %q", InvalidResponseCategory(err))
	}
}
