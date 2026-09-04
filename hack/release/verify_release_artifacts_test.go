// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for verify-release-artifacts.sh.
//
// This script is the only thing standing between a GitHub Release and a deploy
// that trusts it, and it runs on both paths that can publish. It was previously
// covered by a matrix that lived in a pull request description rather than in
// the repository, which is how two defects reached review: a truncated BOM
// image list passed, and the plugin binary the deploy then executed was not
// checked at all.
//
// cosign is stubbed; jq and sha256sum are real, so the payload handling is
// exercised for real.

const (
	verifyTag    = "v9.9.9"
	verifyCommit = "1234567890abcdef1234567890abcdef12345678"
)

// cosignStub records every invocation and fails the ones named in a file, so a
// case can make exactly one verification fail.
const cosignStub = `#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >> "${FIXTURE_DIR}/cosign.log"

# Fail if any argument appears in the fail list, one pattern per line.
if [[ -f "${FIXTURE_DIR}/cosign.fail" ]]; then
  while read -r pattern; do
    [[ -n "$pattern" ]] || continue
    if [[ "$*" == *"$pattern"* ]]; then
      echo "cosign: verification failed for ${pattern}" >&2
      exit 1
    fi
  done < "${FIXTURE_DIR}/cosign.fail"
fi

exit 0
`

// release builds a dist/ directory in the shape `gh release download` leaves.
type release struct {
	t   *testing.T
	dir string
}

func newRelease(t *testing.T) *release {
	t.Helper()

	dir := t.TempDir()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o750); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}

	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	if err := os.WriteFile(filepath.Join(binDir, "cosign"), []byte(cosignStub), 0o700); err != nil {
		t.Fatalf("write cosign stub: %v", err)
	}

	r := &release{t: t, dir: dir}

	// The three signed blobs, each with its bundle.
	r.write("unbounded-manifests-"+verifyTag+".tar.gz", "tarball")
	r.write("unbounded-manifests-"+verifyTag+".tar.gz.bundle.json", "{}")
	r.write("unbounded-operator-"+verifyTag+".yaml", "kind: Deployment")
	r.write("unbounded-operator-"+verifyTag+".yaml.bundle.json", "{}")
	r.writeBOM(map[string]any{
		"release":   map[string]any{"tag": verifyTag, "gitCommit": verifyCommit},
		"artifacts": declaredArtifacts(),
		"images": []any{
			map[string]any{"reference": "ghcr.io/azure/gantry:" + verifyTag, "digest": "sha256:aa"},
			map[string]any{"reference": "ghcr.io/azure/machina:" + verifyTag, "digest": "sha256:bb"},
		},
	})
	r.publishAssets(declaredAssetNames()...)

	return r
}

// declaredArtifacts mirrors hack/cmd/release-bom's list: what a release says it
// shipped, only three of which a deploy ever consumes.
func declaredArtifacts() []any {
	return []any{
		map[string]any{"name": "checksums.txt", "signatureBundle": "checksums.txt.bundle.json"},
		map[string]any{
			"name":            "unbounded-manifests-" + verifyTag + ".tar.gz",
			"signatureBundle": "unbounded-manifests-" + verifyTag + ".tar.gz.bundle.json",
		},
		map[string]any{
			"name":            "unbounded-storage-linux-amd64.tar.gz",
			"signatureBundle": "unbounded-storage-linux-amd64.tar.gz.bundle.json",
		},
		map[string]any{"name": "unbounded.yaml"},
	}
}

func declaredAssetNames() []string {
	return []string{
		"checksums.txt", "checksums.txt.bundle.json",
		"unbounded-manifests-" + verifyTag + ".tar.gz",
		"unbounded-manifests-" + verifyTag + ".tar.gz.bundle.json",
		"unbounded-storage-linux-amd64.tar.gz",
		"unbounded-storage-linux-amd64.tar.gz.bundle.json",
		"unbounded.yaml",
	}
}

// publishAssets writes the list of names the release carries, as
// `gh release view --json assets` would.
func (r *release) publishAssets(names ...string) {
	r.t.Helper()

	r.write("release-assets.txt", strings.Join(names, "\n")+"\n")
}

// withoutAsset republishes the asset list with one name removed, which is what
// a partially uploaded or hand-pruned draft looks like.
func (r *release) withoutAsset(drop string) *release {
	r.t.Helper()

	kept := make([]string, 0, len(declaredAssetNames()))

	for _, name := range declaredAssetNames() {
		if name != drop {
			kept = append(kept, name)
		}
	}

	r.publishAssets(kept...)

	return r
}

func (r *release) path(name string) string { return filepath.Join(r.dir, "dist", name) }

func (r *release) write(name, content string) {
	r.t.Helper()

	if err := os.WriteFile(r.path(name), []byte(content), 0o600); err != nil {
		r.t.Fatalf("write %s: %v", name, err)
	}
}

func (r *release) writeBOM(bom any) {
	r.t.Helper()

	data, err := json.Marshal(bom)
	if err != nil {
		r.t.Fatalf("marshal bom: %v", err)
	}

	r.write("unbounded-release-bom-"+verifyTag+".json", string(data))
	r.write("unbounded-release-bom-"+verifyTag+".json.bundle.json", "{}")
}

// writeRawBOM installs a BOM that is not necessarily valid JSON.
func (r *release) writeRawBOM(content string) {
	r.t.Helper()

	r.write("unbounded-release-bom-"+verifyTag+".json", content)
	r.write("unbounded-release-bom-"+verifyTag+".json.bundle.json", "{}")
}

// withPlugin adds the plugin tarball and a signed checksums.txt covering it,
// which is how a binary is verified: GoReleaser signs the list, not each file.
func (r *release) withPlugin(content string, corrupt bool) *release {
	r.t.Helper()

	name := "kubectl-unbounded-linux-amd64.tar.gz"
	r.write(name, content)

	listed := content
	if corrupt {
		listed = content + " tampered"
	}

	sum := sha256.Sum256([]byte(listed))
	r.write("checksums.txt", fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
	r.write("checksums.txt.bundle.json", "{}")

	return r
}

func (r *release) remove(name string) {
	r.t.Helper()

	if err := os.Remove(r.path(name)); err != nil {
		r.t.Fatalf("remove %s: %v", name, err)
	}
}

// failCosignFor makes any cosign call whose arguments contain pattern fail.
func (r *release) failCosignFor(pattern string) {
	r.t.Helper()

	if err := os.WriteFile(filepath.Join(r.dir, "cosign.fail"), []byte(pattern+"\n"), 0o600); err != nil {
		r.t.Fatalf("write fail list: %v", err)
	}
}

func (r *release) cosignCalls() string {
	r.t.Helper()

	data, err := os.ReadFile(filepath.Join(r.dir, "cosign.log"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}

		r.t.Fatalf("read cosign log: %v", err)
	}

	return string(data)
}

func (r *release) run(env map[string]string) (string, int) {
	r.t.Helper()

	script, err := filepath.Abs("verify-release-artifacts.sh")
	if err != nil {
		r.t.Fatalf("resolve script: %v", err)
	}

	base := map[string]string{
		"PATH":                filepath.Join(r.dir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FIXTURE_DIR":         r.dir,
		"GITHUB_REPOSITORY":   "Azure/unbounded",
		"RELEASE_ASSETS_FILE": filepath.Join(r.dir, "dist", "release-assets.txt"),
	}
	for key, value := range env {
		base[key] = value
	}

	environ := make([]string, 0, len(base))
	for key, value := range base {
		environ = append(environ, key+"="+value)
	}

	command := exec.Command("bash", script, verifyTag, verifyCommit, "dist")
	command.Dir = r.dir
	command.Env = environ

	out, err := command.CombinedOutput()

	code := 0

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		r.t.Fatalf("run script: %v", err)
	}

	return string(out), code
}

func requireVerifier(t *testing.T) {
	t.Helper()

	requireBash4(t)

	for _, tool := range []string{"jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH; skipping verify-release-artifacts.sh tests", tool)
		}
	}
}

func TestVerifierAcceptsACompleteRelease(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)

	output, code := r.run(nil)

	requireCode(t, code, 0, output)
	requireContains(t, output, "2 image(s)")
	// The identity binding is what makes any of this mean anything.
	requireContains(t, r.cosignCalls(), "release\\.yaml@refs/tags/v9\\.9\\.9$")
	requireContains(t, r.cosignCalls(), "ghcr.io/azure/gantry@sha256:aa")
}

func TestVerifierRejectsAMissingArtifact(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.remove("unbounded-manifests-" + verifyTag + ".tar.gz")

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "missing release artifact")
}

func TestVerifierRejectsAMissingBundle(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.remove("unbounded-operator-" + verifyTag + ".yaml.bundle.json")

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "missing signature bundle")
}

func TestVerifierRejectsAnUnverifiableBlob(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.failCosignFor("unbounded-manifests")

	output, code := r.run(nil)

	if code == 0 {
		t.Errorf("expected a failure when a blob signature does not verify\n%s", output)
	}
}

func TestVerifierRejectsATagMismatch(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.writeBOM(map[string]any{
		"release":   map[string]any{"tag": "v0.0.1", "gitCommit": verifyCommit},
		"artifacts": declaredArtifacts(),
		"images":    []any{map[string]any{"reference": "ghcr.io/azure/gantry:v0.0.1", "digest": "sha256:aa"}},
	})

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "records tag v0.0.1")
}

func TestVerifierRejectsACommitMismatch(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.writeBOM(map[string]any{
		"release":   map[string]any{"tag": verifyTag, "gitCommit": "deadbeef"},
		"artifacts": declaredArtifacts(),
		"images":    []any{map[string]any{"reference": "ghcr.io/azure/gantry:" + verifyTag, "digest": "sha256:aa"}},
	})

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "records commit deadbeef")
}

func TestVerifierRejectsAnEmptyImageList(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.writeBOM(map[string]any{
		"release":   map[string]any{"tag": verifyTag, "gitCommit": verifyCommit},
		"artifacts": declaredArtifacts(),
		"images":    []any{},
	})

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "lists no images")
}

// TestVerifierRejectsAPartialImageList is the regression guard for a BOM that
// emits some images and then fails. `mapfile < <(jq ...)` reported mapfile's
// exit status, so the prefix was verified and the release passed.
func TestVerifierRejectsAPartialImageList(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.writeRawBOM(`{"release":{"tag":"` + verifyTag + `","gitCommit":"` + verifyCommit +
		`"},"artifacts":[{"name":"unbounded.yaml"}],` +
		`"images":[{"reference":"ghcr.io/azure/gantry:v9.9.9","digest":"sha256:aa"},"garbage"]}`)

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "refusing to treat it as verified")
	// Nothing may have been accepted on the strength of the prefix.
	requireNotContains(t, output, "OK:")
}

func TestVerifierRejectsAnUnverifiableImage(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.failCosignFor("ghcr.io/azure/machina")

	output, code := r.run(nil)

	if code == 0 {
		t.Errorf("expected a failure when an image signature does not verify\n%s", output)
	}
}

// TestVerifierChecksARequestedBinary covers the artifact the deploy job then
// executes against the cluster. It is covered by the signed checksums.txt
// rather than by a bundle of its own.
func TestVerifierChecksARequestedBinary(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t).withPlugin("plugin bytes", false)

	output, code := r.run(map[string]string{"CHECKSUM_FILES": "kubectl-unbounded-linux-amd64.tar.gz"})

	requireCode(t, code, 0, output)
	requireContains(t, output, "1 checksummed artifact(s)")
	requireContains(t, r.cosignCalls(), "checksums.txt")
}

func TestVerifierRejectsATamperedBinary(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t).withPlugin("plugin bytes", true)

	output, code := r.run(map[string]string{"CHECKSUM_FILES": "kubectl-unbounded-linux-amd64.tar.gz"})

	if code == 0 {
		t.Errorf("expected a failure when the binary does not match checksums.txt\n%s", output)
	}
}

func TestVerifierRejectsAnUnsignedChecksumList(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t).withPlugin("plugin bytes", false)
	r.failCosignFor("checksums.txt")

	output, code := r.run(map[string]string{"CHECKSUM_FILES": "kubectl-unbounded-linux-amd64.tar.gz"})

	if code == 0 {
		t.Errorf("expected a failure when checksums.txt does not verify\n%s", output)
	}
}

// TestVerifierRejectsABinaryMissingFromTheList closes the gap --ignore-missing
// would otherwise leave: an artifact absent from the signed list is
// unverifiable, and sha256sum would simply pass over it.
func TestVerifierRejectsABinaryMissingFromTheList(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t).withPlugin("plugin bytes", false)
	r.write("checksums.txt", "0000  some-other-file.tar.gz\n")

	output, code := r.run(map[string]string{"CHECKSUM_FILES": "kubectl-unbounded-linux-amd64.tar.gz"})

	requireCode(t, code, 1, output)
	requireContains(t, output, "not listed in checksums.txt")
}

func TestVerifierRejectsAMissingChecksumList(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)

	output, code := r.run(map[string]string{"CHECKSUM_FILES": "kubectl-unbounded-linux-amd64.tar.gz"})

	requireCode(t, code, 1, output)
	requireContains(t, output, "checksums.txt")
}

// TestVerifierSkipsChecksumsWhenNoBinaryIsUsed keeps publish-forced, which
// downloads no binaries, from being made to fetch artifacts it never runs.
func TestVerifierSkipsChecksumsWhenNoBinaryIsUsed(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)

	output, code := r.run(nil)

	requireCode(t, code, 0, output)

	if strings.Contains(r.cosignCalls(), "checksums.txt") {
		t.Errorf("did not expect checksums.txt to be verified when no binary was requested\n%s", r.cosignCalls())
	}
}

// TestVerifierRejectsAReleaseMissingADeclaredArtifact is the regression guard
// for a draft that lost assets. The deploy consumes three of the six artifacts
// a release declares; nothing looked at the rest, so a release could publish
// without its storage tarballs, its checksums or unbounded.yaml.
func TestVerifierRejectsAReleaseMissingADeclaredArtifact(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	for _, missing := range []string{
		"unbounded-storage-linux-amd64.tar.gz",
		"unbounded.yaml",
		"checksums.txt",
	} {
		r := newRelease(t).withoutAsset(missing)

		output, code := r.run(nil)

		requireCode(t, code, 1, output)
		requireContains(t, output, missing)
		requireContains(t, output, "it is incomplete")
	}
}

// TestVerifierRejectsAMissingSignatureBundleAsset covers the bundles named
// alongside each artifact: a signature nobody can fetch verifies nothing.
func TestVerifierRejectsAMissingSignatureBundleAsset(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t).withoutAsset("unbounded-storage-linux-amd64.tar.gz.bundle.json")

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "unbounded-storage-linux-amd64.tar.gz.bundle.json")
}

// TestVerifierReportsEveryMissingArtifactAtOnce keeps a half-uploaded draft
// from taking one run per missing file to diagnose.
func TestVerifierReportsEveryMissingArtifactAtOnce(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.publishAssets("checksums.txt", "checksums.txt.bundle.json")

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "unbounded-manifests-"+verifyTag+".tar.gz")
	requireContains(t, output, "unbounded-storage-linux-amd64.tar.gz")
	requireContains(t, output, "unbounded.yaml")
}

// TestVerifierSkipsCompletenessForAnOlderBOM keeps backfilling a tag that
// predates the artifact list working, which is a documented recovery path.
func TestVerifierSkipsCompletenessForAnOlderBOM(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.writeBOM(map[string]any{
		"release": map[string]any{"tag": verifyTag, "gitCommit": verifyCommit},
		"images":  []any{map[string]any{"reference": "ghcr.io/azure/gantry:" + verifyTag, "digest": "sha256:aa"}},
	})

	output, code := r.run(nil)

	requireCode(t, code, 0, output)
	requireContains(t, output, "no artifact list")
}

// TestVerifierRejectsAnEmptyArtifactList separates "this BOM predates the
// field" from "this BOM claims the release shipped nothing".
func TestVerifierRejectsAnEmptyArtifactList(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)
	r.writeBOM(map[string]any{
		"release":   map[string]any{"tag": verifyTag, "gitCommit": verifyCommit},
		"artifacts": []any{},
		"images":    []any{map[string]any{"reference": "ghcr.io/azure/gantry:" + verifyTag, "digest": "sha256:aa"}},
	})

	output, code := r.run(nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "empty artifact list")
}

// TestVerifierRequiresAnAssetListWhenArtifactsAreDeclared refuses to skip the
// check quietly, which is how the gap existed in the first place.
func TestVerifierRequiresAnAssetListWhenArtifactsAreDeclared(t *testing.T) {
	requireVerifier(t)
	t.Parallel()

	r := newRelease(t)

	output, code := r.run(map[string]string{"RELEASE_ASSETS_FILE": ""})

	requireCode(t, code, 2, output)
	requireContains(t, output, "completeness cannot be checked")
}
