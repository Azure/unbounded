// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package awss3

import (
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// makeResponseErr builds an *awshttp.ResponseError wrapping the
// given HTTP status code. Mirrors how the AWS SDK surfaces service
// errors to callers.
func makeResponseErr(status int, inner error) *awshttp.ResponseError {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{
				Response: &http.Response{StatusCode: status},
			},
			Err: inner,
		},
	}
}

// fakeAPIError implements smithy.APIError for testing service-code
// matching paths (AccessDenied / typed-not-found etc).
type fakeAPIError struct{ code string }

func (e *fakeAPIError) Error() string                 { return e.code }
func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.code }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }
func (e *fakeAPIError) HTTPStatusCode() int           { return 0 }

// TestIsPreconditionFailed_FromHTTPStatus verifies that only an HTTP
// 412 response satisfies the predicate. The previous implementation
// matched service codes 'PreconditionFailed' and
// 'ConditionalRequestConflict' plus a substring fallback on
// err.Error(), which was both incomplete (didn't cover backends
// returning only the status) and fragile (false positives on
// arbitrary error messages containing '412').
func TestIsPreconditionFailed_FromHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"412 ResponseError -> true", makeResponseErr(412, errors.New("precondition")), true},
		{"500 ResponseError -> false", makeResponseErr(500, errors.New("ise")), false},
		{"404 ResponseError -> false", makeResponseErr(404, errors.New("not found")), false},
		{"plain error -> false", errors.New("StatusCode: 412 something"), false},
		{"nil -> false", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPreconditionFailed(tt.err); got != tt.want {
				t.Errorf("isPreconditionFailed = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsNotFound covers the typed-error and HTTP-status branches.
func TestIsNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"NoSuchKey typed", &s3types.NoSuchKey{}, true},
		{"NoSuchBucket typed", &s3types.NoSuchBucket{}, true},
		{"NotFound typed", &s3types.NotFound{}, true},
		{"404 ResponseError", makeResponseErr(404, errors.New("nf")), true},
		{"500 ResponseError", makeResponseErr(500, errors.New("ise")), false},
		{"plain error", errors.New("random"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFound(tt.err); got != tt.want {
				t.Errorf("isNotFound = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsAuth covers both the typed APIError branch and the HTTP
// 401/403 status branch.
func TestIsAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"AccessDenied APIError", &fakeAPIError{code: "AccessDenied"}, true},
		{"InvalidAccessKeyId APIError", &fakeAPIError{code: "InvalidAccessKeyId"}, true},
		{"SignatureDoesNotMatch APIError", &fakeAPIError{code: "SignatureDoesNotMatch"}, true},
		{"403 ResponseError", makeResponseErr(403, errors.New("denied")), true},
		{"401 ResponseError", makeResponseErr(401, errors.New("unauth")), true},
		{"404 ResponseError", makeResponseErr(404, errors.New("nf")), false},
		{"500 ResponseError", makeResponseErr(500, errors.New("ise")), false},
		{"plain error", errors.New("auth?"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAuth(tt.err); got != tt.want {
				t.Errorf("isAuth = %v, want %v", got, tt.want)
			}
		})
	}
}
