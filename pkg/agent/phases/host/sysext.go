// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Azure Container Linux mounts /usr read-only and ships no package manager, so
// the agent cannot install systemd-container there the way it does on Ubuntu or
// Azure Linux 3. It is instead delivered as a systemd system extension built by
// images/acl-nspawn-sysext.
//
// systemd-nspawn links systemd's private libsystemd-shared library, and on
// Azure Linux the soname carries the full package release, for example
// libsystemd-shared-255-33.azl3.so. An extension whose systemd does not match
// the host therefore fails at exec with
//
//	systemd-nspawn: error while loading shared libraries:
//	libsystemd-shared-255-27.azl3.so: cannot open shared object file
//
// systemd-sysext does not catch this: it matches only on ID and SYSEXT_LEVEL
// and will happily merge an extension whose binaries cannot load. These checks
// exist to turn that late, confusing runtime failure into an actionable one
// before the extension is merged.

// SysextProvenance describes the systemd build a system extension was made
// from. It is emitted into the extension as
// /usr/lib/extension-release.d/<name>.provenance.
type SysextProvenance struct {
	// Name is the extension name, matching its extension-release suffix.
	Name string
	// SystemdContainerNVR is the systemd-container package the payload came from.
	SystemdContainerNVR string
	// SystemdNVR is the systemd package that supplied libsystemd-shared.
	SystemdNVR string
	// BundledSharedLib is the path, relative to the extension root, of the
	// libsystemd-shared library shipped inside the extension. Empty means the
	// extension relies on the host's copy and is pinned to the host's exact
	// systemd build.
	BundledSharedLib string
}

// Bundled reports whether the extension carries its own libsystemd-shared and
// is therefore not pinned to the host's exact systemd release.
func (p SysextProvenance) Bundled() bool {
	return strings.TrimSpace(p.BundledSharedLib) != ""
}

// ParseSysextProvenance parses the KEY=VALUE provenance emitted by
// images/acl-nspawn-sysext/build-sysext.sh.
func ParseSysextProvenance(content string) (SysextProvenance, error) {
	var p SysextProvenance

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		value = strings.Trim(strings.TrimSpace(value), `"'`)

		switch strings.TrimSpace(key) {
		case "SYSEXT_NAME":
			p.Name = value
		case "SYSTEMD_CONTAINER_NVR":
			p.SystemdContainerNVR = value
		case "SYSTEMD_NVR":
			p.SystemdNVR = value
		case "BUNDLED_SHARED_LIB":
			p.BundledSharedLib = value
		}
	}

	if err := scanner.Err(); err != nil {
		return SysextProvenance{}, fmt.Errorf("read sysext provenance: %w", err)
	}

	if p.SystemdNVR == "" {
		return SysextProvenance{}, fmt.Errorf("sysext provenance is missing SYSTEMD_NVR")
	}

	return p, nil
}

// systemdVersionPattern matches the version-release portion of a systemd
// package NVR ("systemd-255-33.azl3.x86_64") and of the string reported by
// `systemctl --version` ("systemd 255 (255-33.azl3)").
var systemdVersionPattern = regexp.MustCompile(`(?:^|[\s(-])(\d+)-([0-9][^\s)]*)`)

// SystemdRelease is a parsed systemd version-release pair.
type SystemdRelease struct {
	// Major is the upstream systemd version, for example 255.
	Major int
	// Release is the distribution release suffix, for example "33.azl3".
	Release string
}

// String renders the release the way Azure Linux spells it, for example
// "255-33.azl3".
func (r SystemdRelease) String() string {
	return fmt.Sprintf("%d-%s", r.Major, r.Release)
}

// ParseSystemdRelease extracts the version and release from either a systemd
// package NVR or the output of `systemctl --version`.
func ParseSystemdRelease(s string) (SystemdRelease, error) {
	// Trim the architecture suffix so it cannot be mistaken for a release.
	trimmed := strings.TrimSpace(s)
	for _, arch := range []string{".x86_64", ".aarch64", ".noarch"} {
		trimmed = strings.TrimSuffix(trimmed, arch)
	}

	match := systemdVersionPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return SystemdRelease{}, fmt.Errorf("cannot parse systemd release from %q", s)
	}

	major, err := strconv.Atoi(match[1])
	if err != nil {
		return SystemdRelease{}, fmt.Errorf("cannot parse systemd version from %q: %w", s, err)
	}

	return SystemdRelease{Major: major, Release: match[2]}, nil
}

// CheckSysextCompatibility reports whether a system extension can run on a host.
//
// hostVersion is the string reported by `systemctl --version`.
//
// A bundled extension ships its own libsystemd-shared. Because the soname is
// release-qualified, the bundled copy cannot collide with the host's own copy;
// they are distinct filenames in the merged /usr and each binary resolves the
// one it was linked against. Such an extension only needs the host's systemd
// major version to match, so that it keeps talking to PID 1 over a stable
// D-Bus interface and unit-file syntax.
//
// An unbundled extension resolves libsystemd-shared from the host and therefore
// requires the host's systemd release to match exactly.
func CheckSysextCompatibility(p SysextProvenance, hostVersion string) error {
	host, err := ParseSystemdRelease(hostVersion)
	if err != nil {
		return fmt.Errorf("host systemd version: %w", err)
	}

	ext, err := ParseSystemdRelease(p.SystemdNVR)
	if err != nil {
		return fmt.Errorf("extension systemd version: %w", err)
	}

	if host.Major != ext.Major {
		return fmt.Errorf(
			"system extension %q was built against systemd %s but the host runs systemd %s: "+
				"a matching extension must be built for systemd %d",
			p.Name, ext, host, host.Major,
		)
	}

	if !p.Bundled() && host.Release != ext.Release {
		return fmt.Errorf(
			"system extension %q was built against systemd %s but the host runs systemd %s, "+
				"and the extension does not bundle libsystemd-shared: "+
				"rebuild it with BUNDLE_SHARED=1 or against systemd %s",
			p.Name, ext, host, host,
		)
	}

	return nil
}
