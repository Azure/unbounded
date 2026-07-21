// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

func TestParsePullIssues(t *testing.T) {
	raw := []byte(`{"items":[
		{"involvedObject":{"kind":"Pod","name":"job-abc"},"type":"Warning","reason":"Failed","message":"unexpected status: 429 Too Many Requests","count":2},
		{"involvedObject":{"kind":"Pod","name":"job-def"},"type":"Warning","reason":"Failed","message":"503 Egress is over the account limit; failed to authorize: 401 Unauthorized","series":{"count":3}},
		{"involvedObject":{"kind":"Pod","name":"other-abc"},"type":"Warning","reason":"Failed","message":"connection refused","count":9},
		{"involvedObject":{"kind":"Pod","name":"job-ok"},"type":"Normal","reason":"Pulled","message":"pulled","count":1}
	]}`)

	issues, err := parsePullIssues(raw, "job")
	if err != nil {
		t.Fatalf("parsePullIssues: %v", err)
	}

	if !issues.Captured || issues.WarningEvents != 5 || issues.ByReason["Failed"] != 5 {
		t.Fatalf("issues = %+v", issues)
	}

	if issues.Markers["http_429"] != 2 || issues.Markers["http_5xx"] != 3 || issues.Markers["acr_egress_limit"] != 3 || issues.Markers["auth"] != 3 {
		t.Fatalf("markers = %+v", issues.Markers)
	}
}

func TestClassifyPullIssueDoesNotRetainMessage(t *testing.T) {
	message := "GET https://blob.example/data?sig=secret: connection reset by peer"
	markers := classifyPullIssue(message)

	if len(markers) != 1 || markers[0] != "connection_reset" {
		t.Fatalf("markers = %v", markers)
	}
}
