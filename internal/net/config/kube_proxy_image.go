// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ManagedKubeProxyImageFlag is the controller flag that sets the image of the
// managed kube-proxy DaemonSets. The operator passes it on the controller's
// behalf, so the name is shared rather than written out on both sides.
const ManagedKubeProxyImageFlag = "managed-kube-proxy-image"

// The grammar below is the one github.com/distribution/reference implements,
// written out so neither the controller nor the operator takes a dependency
// for one check.
const (
	imagePathComponent   = `[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*`
	imageDomainComponent = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
	imageDomain          = `(?:` + imageDomainComponent + `(?:\.` + imageDomainComponent + `)*|\[[a-fA-F0-9:]+\])(?::[0-9]+)?`

	// maxImageNameLength is the longest repository name a registry accepts.
	maxImageNameLength = 255
)

var (
	imageNamePattern   = regexp.MustCompile(`^(?:` + imageDomain + `/)?` + imagePathComponent + `(?:/` + imagePathComponent + `)*$`)
	imageTagPattern    = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	imageDigestPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[[:xdigit:]]{32,}$`)
)

// ImageReference is a container image reference split into its parts.
type ImageReference struct {
	// Name is the registry and repository, such as registry.k8s.io/kube-proxy.
	Name string

	// Tag is empty when the reference names none.
	Tag string

	// Digest is empty when the reference names none.
	Digest string
}

// HasVersion reports whether the reference pins a tag or a digest.
func (r ImageReference) HasVersion() bool {
	return r.Tag != "" || r.Digest != ""
}

// String reassembles the reference.
func (r ImageReference) String() string {
	out := r.Name

	if r.Tag != "" {
		out += ":" + r.Tag
	}

	if r.Digest != "" {
		out += "@" + r.Digest
	}

	return out
}

// ParseImageReference splits an image reference into its name, tag and digest,
// rejecting anything a registry would not accept.
//
// A colon starts a tag only after the last slash, so the port in
// localhost:5000/kube-proxy belongs to the name and the reference has no
// version.
func ParseImageReference(ref string) (ImageReference, error) {
	if ref == "" {
		return ImageReference{}, errors.New("image reference is empty")
	}

	var parsed ImageReference

	name := ref

	if at := strings.Index(name, "@"); at >= 0 {
		parsed.Digest = name[at+1:]
		name = name[:at]

		if !imageDigestPattern.MatchString(parsed.Digest) {
			return ImageReference{}, fmt.Errorf("image reference %q has a malformed digest %q", ref, parsed.Digest)
		}
	}

	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		parsed.Tag = name[colon+1:]
		name = name[:colon]

		if !imageTagPattern.MatchString(parsed.Tag) {
			return ImageReference{}, fmt.Errorf("image reference %q has a malformed tag %q", ref, parsed.Tag)
		}
	}

	if len(name) > maxImageNameLength {
		return ImageReference{}, fmt.Errorf("image reference %q has a name longer than %d characters", ref, maxImageNameLength)
	}

	if !imageNamePattern.MatchString(name) {
		return ImageReference{}, fmt.Errorf(
			"image reference %q has a malformed name %q; repository paths are lowercase letters, digits and separators",
			ref, name,
		)
	}

	parsed.Name = name

	return parsed, nil
}
