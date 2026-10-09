// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type s3Options struct {
	Endpoint string
	Bucket   string
	Count    int
	Bytes    int64
	Origin   bool
}

func configureS3Options(opts *options, seen map[string]bool) error {
	if opts.pull.Backend != "s3" {
		for _, name := range []string{"endpoint", "bucket", "object-count", "object-bytes", "s3-origin"} {
			if seen[name] {
				return fmt.Errorf("%s requires backend=s3", name)
			}
		}

		return nil
	}

	for _, name := range []string{"target", "namespace", "volume", "repository", "catalog-blobs", "blob-bytes", "catalog-images", "layers", "layer-bytes", "jitter", "blob-concurrency"} {
		if seen[name] {
			return fmt.Errorf("backend=s3 cannot be combined with %s", name)
		}
	}

	if !seen["endpoint"] {
		opts.s3.Endpoint = "http://127.0.0.1:8080"
	}

	if !seen["bucket"] {
		opts.s3.Bucket = "benchmark"
	}

	if !seen["object-count"] {
		opts.s3.Count = 128
	}

	if !seen["object-bytes"] {
		opts.s3.Bytes = 64 << 20
	}

	if opts.s3.Count < 1 || opts.s3.Count > maxCatalogImages || opts.s3.Bytes < 1 {
		return errors.New("object-count must be in [1, 512] and object-bytes must be positive")
	}

	if !validS3Bucket(opts.s3.Bucket) {
		return errors.New("invalid S3 bucket")
	}

	u, err := url.Parse(opts.s3.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(opts.s3.Endpoint, "#") || (u.Path != "" && u.Path != "/") {
		return errors.New("endpoint must be an HTTP(S) origin without credentials, path prefix, query, or fragment")
	}

	if strings.HasSuffix(u.Host, ":") {
		return errors.New("invalid endpoint port")
	}

	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid endpoint port")
		}
	}

	opts.pull.Target = opts.s3.Endpoint
	opts.pull.BlobConcurrency = 1

	return nil
}

// validS3Bucket accepts the portable path-style subset of S3 bucket names:
// 3-63 lowercase letters, digits, dots, and hyphens, starting and ending with
// a letter or digit, without adjacent dots.
func validS3Bucket(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > 63 || strings.Contains(bucket, "..") {
		return false
	}

	alnum := func(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }
	for i := range len(bucket) {
		if c := bucket[i]; !alnum(c) && c != '.' && c != '-' {
			return false
		}
	}

	return alnum(bucket[0]) && alnum(bucket[len(bucket)-1])
}

func s3ObjectKey(index int) string { return fmt.Sprintf("object-%06d", index) }

// newS3Client builds a standard anonymous, path-style S3 client over the
// puller's HTTP client. The SDK must not retry or follow redirects so that
// every request and failure is reported exactly once.
func (p *puller) newS3Client() *s3.Client {
	return s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(strings.TrimSuffix(p.target.String(), "/")),
		UsePathStyle:               true,
		Credentials:                aws.AnonymousCredentials{},
		HTTPClient:                 p.client,
		Retryer:                    aws.NopRetryer{},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

// Keep S3 ETags opaque to the consumer: integrity comes from the expected SHA-256,
// not from assuming the upstream uses MD5 or our synthetic ETag format.
func (p *puller) configureS3(catalog *blobCatalog, bucket string) {
	keys := make(map[digest.Digest]string, len(catalog.batches))
	for index, batch := range catalog.batches {
		keys[batch.blobs[0].descriptor.Digest] = s3ObjectKey(index)
	}

	client := p.newS3Client()
	p.acquire = func(ctx context.Context, _ string, desc ocispec.Descriptor) (blobResponse, error) {
		key, ok := keys[desc.Digest]
		if !ok {
			return blobResponse{}, errors.New("S3 object missing from catalog")
		}

		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			return blobResponse{}, err
		}

		// The SDK accepts any 2xx; only a full 200 response is a complete object.
		status := http.StatusOK
		if raw, ok := awsmiddleware.GetRawResponse(out.ResultMetadata).(*smithyhttp.Response); ok {
			status = raw.StatusCode
		}

		return blobResponse{body: out.Body, status: status, success: status == http.StatusOK}, nil
	}
}

// s3ErrorStatus reports the HTTP status of an S3 error response, or zero when
// the request failed before a response was received.
func s3ErrorStatus(err error) int {
	var response *awshttp.ResponseError
	if errors.As(err, &response) {
		return response.HTTPStatusCode()
	}

	return 0
}

// This is a read-only, unauthenticated S3 fixture, not an object store. The
// virtual ReaderAt regenerates ranges with memory independent of object size.
func (c *blobCatalog) s3Handler(bucket string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			s3Error(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")

			return
		}

		pathBucket, key, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if pathBucket != bucket {
			s3Error(w, r, http.StatusNotFound, "NoSuchBucket")
			return
		}

		index, err := strconv.Atoi(strings.TrimPrefix(key, "object-"))
		if !found || err != nil || index < 0 || index >= len(c.batches) || key != s3ObjectKey(index) {
			s3Error(w, r, http.StatusNotFound, "NoSuchKey")
			return
		}

		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			s3Error(w, r, http.StatusBadRequest, "InvalidArgument")
			return
		}

		for name, values := range query {
			if name == "versionId" {
				s3Error(w, r, http.StatusNotFound, "NoSuchVersion")
				return
			}

			want := "GetObject"
			if r.Method == http.MethodHead {
				want = "HeadObject"
			}

			if name != "x-id" || len(values) != 1 || values[0] != want {
				s3Error(w, r, http.StatusNotImplemented, "NotImplemented")
				return
			}
		}

		for _, header := range []string{"If-Modified-Since", "If-Unmodified-Since", "If-Range"} {
			if r.Header.Get(header) != "" {
				s3Error(w, r, http.StatusNotImplemented, "NotImplemented")
				return
			}
		}

		blob := c.blobs[c.batches[index].blobs[0].descriptor.Digest]
		etag := `"` + blob.descriptor.Digest.String() + `"`
		w.Header().Set("ETag", etag)

		if match := r.Header.Get("If-Match"); match != "" && !s3ETagMatches(match, etag, false) {
			s3Error(w, r, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}

		if none := r.Header.Get("If-None-Match"); none != "" && s3ETagMatches(none, etag, true) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		size := blob.descriptor.Size

		first, length, err := s3Range(r.Header.Get("Range"), size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			s3Error(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")

			return
		}

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))

		status := http.StatusOK
		if r.Header.Get("Range") != "" {
			status = http.StatusPartialContent

			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, first+length-1, size))
		}

		w.WriteHeader(status)

		if r.Method == http.MethodGet {
			if _, err := io.Copy(w, io.NewSectionReader(blob.data, first, length)); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
	})
}

func s3ETagMatches(value, etag string, weak bool) bool {
	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(item)
		if weak {
			item = strings.TrimPrefix(item, "W/")
		}

		if item == "*" || item == etag {
			return true
		}
	}

	return false
}

func s3Range(value string, size int64) (int64, int64, error) {
	if value == "" {
		return 0, size, nil
	}

	raw, ok := strings.CutPrefix(value, "bytes=")
	left, right, cut := strings.Cut(raw, "-")

	invalid := errors.New("invalid byte range")
	if !ok || !cut || size == 0 {
		return 0, 0, invalid
	}

	parse := func(s string) (int64, error) {
		if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, invalid
		}

		return strconv.ParseInt(s, 10, 64)
	}
	if left == "" {
		n, err := parse(right)
		if err != nil || n == 0 {
			return 0, 0, invalid
		}

		n = min(n, size)

		return size - n, n, nil
	}

	first, err := parse(left)
	if err != nil || first >= size {
		return 0, 0, invalid
	}

	last := size - 1
	if right != "" {
		last, err = parse(right)
		if err != nil || last < first {
			return 0, 0, invalid
		}

		last = min(last, size-1)
	}

	return first, last - first + 1, nil
}

func s3Error(w http.ResponseWriter, r *http.Request, status int, code string) {
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: http.StatusText(status)})
	if err != nil {
		panic(http.ErrAbortHandler)
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)

	if r.Method != http.MethodHead {
		if _, err := w.Write(body); err != nil {
			return
		}
	}
}
