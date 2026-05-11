// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package s3

import (
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Azure/unbounded/internal/orca/cachestore"
)

// makeResponseErr builds an *awshttp.ResponseError wrapping the
// given HTTP status code. Mirrors how the AWS SDK surfaces service
// errors to callers: an *awshttp.ResponseError nesting a
// *smithyhttp.ResponseError that carries the HTTP response.
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

// TestIsPreconditionFailed_FromHTTPStatus verifies that 412 alone
// signals precondition failure; other statuses (and errors lacking
// HTTP-response context) do not. The original implementation matched
// service error codes by string ("PreconditionFailed",
// "InvalidArgument", "ConditionalRequestConflict") plus substring
// "412" - fragile across SDK versions and backend implementations.
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
		{"404 ResponseError", makeResponseErr(404, errors.New("not found")), true},
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

// fakeAPIError implements smithy.APIError for testing the
// AccessDenied / Forbidden mapping path.
type fakeAPIError struct{ code string }

func (e *fakeAPIError) Error() string                 { return e.code }
func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.code }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }
func (e *fakeAPIError) HTTPStatusCode() int           { return 0 }

// TestMapErr covers the full mapping table: 404 / typed not-found
// -> ErrNotFound, AccessDenied APIError -> ErrAuth, 5xx ->
// ErrTransient, anything else passes through.
func TestMapErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"NoSuchKey -> ErrNotFound", &s3types.NoSuchKey{}, cachestore.ErrNotFound},
		{"404 ResponseError -> ErrNotFound", makeResponseErr(404, errors.New("nf")), cachestore.ErrNotFound},
		{"AccessDenied APIError -> ErrAuth", &fakeAPIError{code: "AccessDenied"}, cachestore.ErrAuth},
		{"InvalidAccessKeyId APIError -> ErrAuth", &fakeAPIError{code: "InvalidAccessKeyId"}, cachestore.ErrAuth},
		{"403 ResponseError -> ErrAuth", makeResponseErr(403, errors.New("denied")), cachestore.ErrAuth},
		{"401 ResponseError -> ErrAuth", makeResponseErr(401, errors.New("unauth")), cachestore.ErrAuth},
		{"500 ResponseError -> ErrTransient", makeResponseErr(500, errors.New("ise")), cachestore.ErrTransient},
		{"503 ResponseError -> ErrTransient", makeResponseErr(503, errors.New("unavail")), cachestore.ErrTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErr(tt.err)
			if !errors.Is(got, tt.want) {
				t.Errorf("mapErr = %v, want errors.Is(_, %v) true", got, tt.want)
			}
		})
	}
}

// TestMapErr_PassthroughUnknown verifies that unrecognized errors
// pass through unchanged.
func TestMapErr_PassthroughUnknown(t *testing.T) {
	t.Parallel()

	src := errors.New("unrecognized")
	if got := mapErr(src); got != src {
		t.Errorf("mapErr(unknown) = %v, want passthrough %v", got, src)
	}
}
