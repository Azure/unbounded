// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package manifests holds tests that validate the orca deployment
// manifest templates render to syntactically correct, structurally
// reasonable Kubernetes YAML.
//
// These tests catch typos, missing required fields, and template
// regressions at compile time without needing a Kind cluster. They
// complement (but do not replace) hack/orca's actual `kubectl apply`
// validation.
package manifests
