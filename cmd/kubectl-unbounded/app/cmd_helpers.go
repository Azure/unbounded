// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"os"
	"path/filepath"
)

func getKubeconfigPath(p string) string {
	if !isEmpty(p) {
		return p
	}

	if env := os.Getenv("KUBECONFIG"); !isEmpty(env) {
		return env
	}

	// Fall back to the default kubectl kubeconfig location.
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "config")
	}

	return ""
}
