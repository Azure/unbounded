// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package license

import (
	"fmt"
	"strings"
)

// BuildURL combines the repository base, ref, and license filename into a
// canonical blob URL. GitHub and GitLab share the `/blob/<ref>/<file>`
// shape; cs.opensource.google uses `/+/refs/tags/<ref>:<file>`; bitbucket
// uses `/src/<ref>/<file>`. Unknown forges fall through to the bare base URL.
func BuildURL(base, ref, licenseFile string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("empty repository base")
	}

	switch {
	case strings.HasPrefix(base, "https://github.com/"):
		return fmt.Sprintf("%s/blob/%s/%s", base, ref, licenseFile), nil
	case strings.HasPrefix(base, "https://gitlab.com/"):
		return fmt.Sprintf("%s/-/blob/%s/%s", base, ref, licenseFile), nil
	case strings.HasPrefix(base, "https://cs.opensource.google/"):
		return fmt.Sprintf("%s/+/refs/tags/%s:%s", base, ref, licenseFile), nil
	case strings.HasPrefix(base, "https://bitbucket.org/"):
		return fmt.Sprintf("%s/src/%s/%s", base, ref, licenseFile), nil
	default:
		return base, nil
	}
}
