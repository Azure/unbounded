// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"strings"
	"testing"
)

var writeKey = func() [32]byte {
	var k [32]byte

	k[31] = 0xab

	return k
}()

const writeKeyHex = "00000000000000000000000000000000000000000000000000000000000000ab"

func TestWriteHeadGolden(t *testing.T) {
	r := Request{Key: writeKey, AdapterMetadata: `{"schema":1,"bucket":"b","key":"k"}`, Authorization: "Bearer t"}
	cases := []struct {
		kind string
		o    WriteOptions
		want string
	}{
		{WritePut, WriteOptions{Length: 5}, "PUT /v1/objects/" + writeKeyHex + " HTTP/1.1\r\nHost: racer\r\nContent-Length: 5\r\nIf-None-Match: *\r\n" +
			"Racer-Metadata: {\"schema\":1,\"bucket\":\"b\",\"key\":\"k\"}\r\nAuthorization: Bearer t\r\n\r\n"},
		{
			WritePut,
			WriteOptions{Length: 5, Replicas: 2, Upload: UploadNever, HoldSeconds: 60, ContentType: "a/b"},
			"PUT /v1/objects/" + writeKeyHex + " HTTP/1.1\r\nHost: racer\r\nContent-Length: 5\r\nIf-None-Match: *\r\n" +
				"Racer-Durability: replicas=2\r\nRacer-Upload: never\r\nRacer-Hold-Seconds: 60\r\nRacer-Content-Type: a/b\r\n" +
				"Racer-Metadata: {\"schema\":1,\"bucket\":\"b\",\"key\":\"k\"}\r\nAuthorization: Bearer t\r\n\r\n",
		},
		{
			WriteHostWrite,
			WriteOptions{Length: 7, SourceOffset: 4096, OriginDurable: true},
			"POST /v1/objects/" + writeKeyHex + "/host-write HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nIf-None-Match: *\r\n" +
				"Racer-Source-Offset: 4096\r\nRacer-Source-Length: 7\r\nRacer-Durability: origin\r\n" +
				"Racer-Metadata: {\"schema\":1,\"bucket\":\"b\",\"key\":\"k\"}\r\nAuthorization: Bearer t\r\n\r\n",
		},
		{WritePersist, WriteOptions{}, "POST /v1/objects/" + writeKeyHex + "/persist HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n" +
			"Racer-Metadata: {\"schema\":1,\"bucket\":\"b\",\"key\":\"k\"}\r\nAuthorization: Bearer t\r\n\r\n"},
		{WriteDelete, WriteOptions{}, "DELETE /v1/objects/" + writeKeyHex + " HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n" +
			"Racer-Metadata: {\"schema\":1,\"bucket\":\"b\",\"key\":\"k\"}\r\nAuthorization: Bearer t\r\n\r\n"},
	}

	for _, tc := range cases {
		got, err := WriteHead(tc.kind, r, tc.o)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}

		if string(got) != tc.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tc.kind, got, tc.want)
		}
	}
}

func TestWriteHeadRejects(t *testing.T) {
	r := Request{Key: writeKey}
	cases := []struct {
		name string
		kind string
		o    WriteOptions
		r    Request
	}{
		{"zero length", WritePut, WriteOptions{}, r},
		{"too large", WritePut, WriteOptions{Length: MaxObjectBytes + 1}, r},
		{"host zero", WriteHostWrite, WriteOptions{}, r},
		{"host overflow", WriteHostWrite, WriteOptions{Length: 2, SourceOffset: 1<<63 - 2}, r},
		{"replicas", WritePut, WriteOptions{Length: 1, Replicas: 4}, r},
		{"negative replicas", WritePut, WriteOptions{Length: 1, Replicas: -1}, r},
		{"origin manual", WritePut, WriteOptions{Length: 1, OriginDurable: true, Upload: UploadManual}, r},
		{"origin replicas", WritePut, WriteOptions{Length: 1, OriginDurable: true, Replicas: 1}, r},
		{"upload", WritePut, WriteOptions{Length: 1, Upload: "x"}, r},
		{"hold", WritePut, WriteOptions{Length: 1, HoldSeconds: MaxHoldSeconds + 1}, r},
		{"content type", WritePut, WriteOptions{Length: 1, ContentType: "a\x01"}, r},
		{"kind", "get", WriteOptions{}, r},
		{"version", WritePut, WriteOptions{Length: 1}, Request{Key: writeKey, AdapterMetadata: `{"versionId":"1"}`}},
		{"bad metadata", WritePut, WriteOptions{Length: 1}, Request{Key: writeKey, AdapterMetadata: " x"}},
	}

	for _, tc := range cases {
		if _, err := WriteHead(tc.kind, tc.r, tc.o); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestHasVersionID(t *testing.T) {
	for in, want := range map[string]bool{
		`{"schema":1,"bucket":"b","key":"k"}`:                 false,
		`{"schema":1,"bucket":"b","key":"k","versionId":"v"}`: true,
		`not json`:      false,
		`["versionId"]`: false,
	} {
		if HasVersionID(in) != want {
			t.Errorf("%s", in)
		}
	}
}

func TestParseWriteResponse(t *testing.T) {
	etag := `"racer-0123456789abcdef0123456789abcdef"`
	ok := []struct {
		kind, head string
		want       WriteResponse
	}{
		{
			WritePut, "HTTP/1.1 201 Created\r\nETag: " + etag + "\r\nRacer-Upload-State: pending\r\nContent-Length: 0\r\n\r\n",
			WriteResponse{Status: 201, ETag: etag, UploadState: StatePending, RetryAfter: -1},
		},
		{
			WritePersist, "HTTP/1.1 204 No Content\r\nRacer-Upload-State: pending\r\n\r\n",
			WriteResponse{Status: 204, UploadState: StatePending, RetryAfter: -1},
		},
		{WriteDelete, "HTTP/1.1 204 No Content\r\n\r\n", WriteResponse{Status: 204, RetryAfter: -1}},
		{
			WritePut, "HTTP/1.1 503 Busy\r\nContent-Length: 0\r\nRetry-After: 3\r\nRacer-Write-Error: dirty-cap\r\n\r\n",
			WriteResponse{Status: 503, Error: "dirty-cap", RetryAfter: 3},
		},
		{WritePut, "HTTP/1.1 412 X\r\nContent-Length: 0\r\nRetry-After: soon\r\n\r\n", WriteResponse{Status: 412, RetryAfter: -1}},
	}

	for _, tc := range ok {
		got, err := ParseWriteResponse([]byte(tc.head), tc.kind)
		if err != nil || got != tc.want {
			t.Errorf("%q: %+v %v", tc.head, got, err)
		}
	}

	bad := []struct{ kind, head string }{
		{WritePut, "HTTP/1.1 201 Created\r\nRacer-Upload-State: pending\r\nContent-Length: 0\r\n\r\n"},
		{WritePut, "HTTP/1.1 201 Created\r\nETag: " + etag + "\r\nContent-Length: 0\r\n\r\n"},
		{WritePut, "HTTP/1.1 201 Created\r\nETag: " + etag + "\r\nRacer-Upload-State: bogus\r\nContent-Length: 0\r\n\r\n"},
		{WritePut, "HTTP/1.1 201 Created\r\nETag: " + etag + "\r\nRacer-Upload-State: pending\r\nContent-Length: 3\r\n\r\n"},
		{WritePut, "HTTP/1.1 201 Created\r\nETag: " + etag + "\r\nRacer-Upload-State: pending\r\n\r\n"},
		{WritePut, "HTTP/1.1 204 No Content\r\n\r\n"},
		{WriteDelete, "HTTP/1.1 201 Created\r\nETag: " + etag + "\r\nRacer-Upload-State: pending\r\nContent-Length: 0\r\n\r\n"},
		{WritePersist, "HTTP/1.1 204 No Content\r\n\r\n"},
		{WritePut, "HTTP/1.1 503 X\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\n\r\n"},
		{WritePut, "HTTP/1.0 201 Created\r\n\r\n"},
		{WritePut, "HTTP/1.1 2x1 Created\r\n\r\n"},
		{WritePut, "HTTP/1.1 201 Created\r\n"},
	}

	for _, tc := range bad {
		if _, err := ParseWriteResponse([]byte(tc.head), tc.kind); err == nil {
			t.Errorf("accepted %q", tc.head)
		}
	}
}

func TestClientHeadUploadState(t *testing.T) {
	r := Request{Key: writeKey, Operation: OperationHead}
	base := "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 3\r\n"

	got, err := ParseClientHeadResponse([]byte(base+"Racer-Upload-State: never\r\n\r\n"), r)
	if err != nil || got.UploadState != StateNever {
		t.Fatalf("%+v %v", got, err)
	}

	if _, err := ParseClientHeadResponse([]byte(base+"Racer-Upload-State: nope\r\n\r\n"), r); err == nil || !strings.Contains(err.Error(), "response") {
		t.Fatalf("accepted bad state: %v", err)
	}

	got, err = ParseClientHeadResponse([]byte(base+"\r\n"), r)
	if err != nil || got.UploadState != "" {
		t.Fatalf("%+v %v", got, err)
	}
}
