// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// executableFile reports whether path exists, is a regular file, and has any
// execute bit set. It is the "provider binary already in place" check that
// lets the steady-state and external-policy paths skip the offline-bundle
// fetch/verify entirely.
func executableFile(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}

	return !s.IsDir() && s.Mode()&0o111 != 0
}

// writeFileIfChanged atomically writes content to path and reports whether the
// on-disk bytes differed, so the caller knows whether the kubelet must be
// restarted to pick the change up.
func writeFileIfChanged(path string, content []byte, perm os.FileMode) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}

	if os.IsNotExist(err) || !bytesEqual(existing, content) {
		tmp, err := os.CreateTemp(filepath.Dir(path), ".registry-auth-*")
		if err != nil {
			return false, err
		}

		name := tmp.Name()
		defer os.Remove(name) //nolint:errcheck // best-effort cleanup of the temp file

		if _, err := tmp.Write(content); err != nil {
			tmp.Close() //nolint:errcheck // best-effort cleanup; the write error is the one to report

			return false, err
		}

		if err := tmp.Chmod(perm); err != nil {
			tmp.Close() //nolint:errcheck // best-effort cleanup; the chmod error is the one to report

			return false, err
		}

		// Sync the content before the rename so the file data is durable.
		if err := fsyncFile(tmp); err != nil {
			tmp.Close() //nolint:errcheck // best-effort cleanup; the sync error is the one to report

			return false, err
		}

		if err := tmp.Close(); err != nil {
			return false, err
		}

		if err := os.Rename(name, path); err != nil {
			return false, err
		}

		// Sync the destination directory after the rename so the final entry is
		// durable, not just the temporary name.
		if err := fsyncDir(path); err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// registryAuthNodeStart re-resolves the node-stack goal state for the active
// machine's slot, applying the mode-aware root the running node uses. It is the
// same root the repave/machine-op path places files through, so placement and
// teardown target the correct filesystem in every isolation mode:
//   - none: the host (identity root, bin/libexec/cni redirected into the
//
// version directory),
//   - container: the container's rootfs,
//   - nspawn: the machine directory (the default goal-state root).
//
// The preflight resolver is used so the placement does not require device
// readiness, and no disruptive Prepare is invoked just to discover a root.
//
// The offline-artifact bundle is intentionally NOT resolved here: resolution
// fetches and verifies the entire bundle, which would force a bootstrap source
// to remain available for every reconcile pass, including pure verification and
// teardown of an external policy that never downloads the provider. The
// provider download (ensureACRProviderBinary) resolves the bundle lazily, only
// when the binary actually needs to be fetched.
func registryAuthNodeStart(log *slog.Logger, active *ActiveMachine) (*goalstates.MachineGoalState, error) {
	gs, err := goalstates.ResolveMachineForPreflight(log, active.Config, active.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve node stack root for %s: %w", active.Name, err)
	}

	switch activeModeOf(active) {
	case isolation.ModeNone:
		gs.NodeStart.RootOverride = none.Root{}
	case isolation.ModeContainer:
		root, err := container.NewDefault(log).NodeRoot(active.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve container root for %s: %w", active.Name, err)
		}

		gs.NodeStart.RootOverride = root
	}

	return gs, nil
}

// ensureACRProviderBinary downloads the stock ACR credential provider into the
// node stack if it is not already present. It reuses the rootfs download task so
// the binary lands and is verified exactly as it would during provisioning, and
// it drives the download through the node's configured source (including an
// offline mirror) rather than always the default upstream URL, because the
// provider is no longer supplied by an installer image and air-gapped nodes are
// the motivating environment.
//
// The offline bundle is resolved only when a download is actually needed. A node
// that already carries the provider binary (the common steady state) or one
// converging to an external policy does not need the bootstrap source to remain
// available, so verification and teardown do not couple to it.
func ensureACRProviderBinary(ctx context.Context, log *slog.Logger, active *ActiveMachine, gs *goalstates.MachineGoalState) error {
	// The download task writes through the node stack root. Point it at the
	// resolved root so it places the binary where the running stack expects it.
	gs.RootFS.RootOverride = gs.NodeStart.Root()

	// A download is only needed when the provider binary is not already in
	// place. Checking first lets the steady-state and external-policy paths skip
	// the offline-bundle fetch/verify entirely.
	target := gs.RootFS.Root().Path(filepath.Join(goalstates.RegistryAuthProviderBinDir, goalstates.RegistryAuthProviderName))
	if executableFile(target) {
		return nil
	}

	downloads, _, err := goalstates.ResolveDownloadOverridesWithOfflineArtifacts(ctx, active.Config, nil)
	if err != nil {
		return fmt.Errorf("resolve download overrides for %s: %w", active.Name, err)
	}

	if downloads != nil {
		gs.RootFS.Downloads = downloads
	}

	return rootfs.DownloadACRProvider(log, gs.RootFS).Do(ctx)
}

// registryAuthKubeletUnitName is the kubelet systemd unit name for the mode.
func registryAuthKubeletUnitName(mode isolation.Mode) string {
	switch mode {
	case isolation.ModeNone:
		return "unbounded-kubelet.service"
	default:
		return goalstates.SystemdUnitKubelet
	}
}

// registryAuthNodeConfigDropIn is the path (through the node-stack root) of the
// kubelet node-config drop-in that renders $KUBELET_NODE_CONFIG_ARGS, where the
// managed image credential provider flags live. The kubelet unit's ExecStart
// references $KUBELET_NODE_CONFIG_ARGS, so this is the effective, mode-correct
// place to enable or disable the provider without restating ExecStart.
func registryAuthNodeConfigDropIn(root isolation.NodeRoot, mode isolation.Mode) string {
	return root.Path(filepath.Join(goalstates.SystemdSystemDir, registryAuthKubeletUnitName(mode)+".d", "20-node-config.conf"))
}

// registryAuthProviderFlags are the kubelet image credential provider flags the
// agent toggles in $KUBELET_NODE_CONFIG_ARGS.
func registryAuthProviderFlags() []string {
	return []string{
		"--image-credential-provider-config=" + goalstates.RegistryAuthCredentialConfigPath,
		"--image-credential-provider-bin-dir=" + goalstates.RegistryAuthProviderBinDir,
	}
}

// registryAuthNodeConfigDropInPath resolves the node-config drop-in path. It is
// a variable so tests can point it at a temporary path instead of the node
// stack root.
var registryAuthNodeConfigDropInPath = func(root isolation.NodeRoot, mode isolation.Mode) string {
	return registryAuthNodeConfigDropIn(root, mode)
}

// applyProviderToNodeConfig adds the managed provider flags to the kubelet
// node-config drop-in's $KUBELET_NODE_CONFIG_ARGS if they are absent, or removes
// them when policy is nil. It edits only the provider tokens, preserving the
// other effective node config args (hostname-override, node-ip, labels) and any
// other existing drop-ins. It reports whether the on-disk content changed, which
// is what triggers a kubelet restart.
func applyProviderToNodeConfig(root isolation.NodeRoot, mode isolation.Mode, policy *registryauth.Config) (bool, error) {
	path := registryAuthNodeConfigDropInPath(root, mode)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if policy == nil {
				// No node-config drop-in and nothing to remove: the kubelet's
				// authentication is already external.
				return false, nil
			}

			return false, fmt.Errorf("kubelet node-config drop-in %s not found; the node stack has not written it yet", path)
		}

		return false, fmt.Errorf("read kubelet node-config drop-in %s: %w", path, err)
	}

	envValue, err := nodeConfigArgsValue(string(data))
	if err != nil {
		return false, err
	}

	args := registryAuthProviderArgs(envValue)
	if policy == nil {
		removed := args.removeManaged()
		if !removed {
			return false, nil
		}

		// Re-render the complete original drop-in so only the provider tokens
		// change; the header comment, [Service] stanza, and any other
		// Environment= directives are preserved byte for byte.
		return writeFileIfChanged(path, []byte(nodeConfigArgsRendered(string(data), args)), 0o644)
	}

	added, err := args.addManaged()
	if err != nil {
		return false, err
	}

	if !added {
		return false, nil
	}

	return writeFileIfChanged(path, []byte(nodeConfigArgsRendered(string(data), args)), 0o644)
}

// The systemd Environment= value grammar is a list of whitespace-separated
// items, where each item is an unquoted run of non-whitespace or a quoted
// string that may itself contain whitespace. Quoted items may be double-quoted
// (C-style backslash escapes: \\, \", \n, \t, and octal; a literal backslash is
// doubled) or single-quoted (everything literal except the closing quote). A
// single Environment= directive can carry several items, so one line can hold
// multiple assignments.
//
// These helpers implement that grammar so the reconciler parses and re-renders
// the KUBELET_NODE_CONFIG_ARGS entry correctly regardless of how the drop-in
// quotes it, and preserves every unrelated item byte for byte.

// systemdEnvItems tokenizes the value of one Environment= directive into its
// decoded items. A quoted item (single or double) is returned with its quotes
// removed and, for double quotes, its C-style backslash escapes unescaped; an
// unquoted item is returned verbatim. Whitespace between items is a separator;
// whitespace inside a quoted item is preserved.
func systemdEnvItems(value string) []string {
	var (
		items []string
		cur   strings.Builder
	)

	inQuote := byte(0)
	quoted := false

	for i := 0; i < len(value); i++ {
		c := value[i]

		switch {
		case inQuote != 0:
			if inQuote == '"' && c == '\\' && i+1 < len(value) {
				decoded, consumed, ok := systemdDecodeEscape(value[i:])
				if ok {
					cur.WriteRune(decoded)

					i += consumed - 1

					continue
				}
				// An unrecognized escape: keep the backslash literally.
				cur.WriteByte(c)

				continue
			}

			if c == inQuote {
				inQuote = 0

				continue
			}

			cur.WriteByte(c)

		case c == '"' || c == '\'':
			inQuote = c
			quoted = true

		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			if cur.Len() > 0 || quoted {
				items = append(items, cur.String())
				cur.Reset()
			}

			quoted = false

		default:
			cur.WriteByte(c)
		}
	}

	if cur.Len() > 0 || quoted {
		items = append(items, cur.String())
	}

	return items
}

// systemdDecodeEscape resolves a C-style escape sequence that follows a backslash
// inside a double-quoted systemd item. It returns the decoded rune and the number
// of bytes consumed after the backslash (including the backslash itself). It
// reports ok=false when the sequence is not recognized; the caller then decides
// whether to treat it as a literal backslash or an error.
//
// Supported forms (per systemd.syntax man page, "Quoting" section):
//   - \n, \t, \r, \b, \f, \v, \\, \", \'  (single-char C escapes)
//   - \xHH  (hex, 1-2 hex digits)
//   - \NNN  (octal, 1-3 octal digits)
//   - \uNNNN (Unicode BMP, 4 hex digits)
//   - \UNNNNNNNN (Unicode full, 8 hex digits)
//   - \s  (space; systemd also allows \s for a literal space)
func systemdDecodeEscape(s string) (rune, int, bool) {
	if len(s) < 2 {
		return 0, 0, false
	}

	c := s[1]

	switch c {
	case '\\', '"', '\'':
		return rune(c), 2, true
	case 'n':
		return '\n', 2, true
	case 't':
		return '\t', 2, true
	case 'r':
		return '\r', 2, true
	case 'b':
		return '\b', 2, true
	case 'f':
		return '\f', 2, true
	case 'v':
		return '\v', 2, true
	case 's':
		return ' ', 2, true
	case 'x':
		// Hex: \xHH (1-2 hex digits).
		hex := s[2:]

		n := 0
		for n < len(hex) && n < 2 && isHexDigit(hex[n]) {
			n++
		}

		if n == 0 {
			return 0, 0, false
		}

		v, err := strconv.ParseUint(hex[:n], 16, 32)
		if err != nil {
			return 0, 0, false
		}

		return rune(v), 2 + n, true
	}

	// Octal: \NNN (1-3 octal digits).
	if c >= '0' && c <= '7' {
		oct := s[1:]

		n := 0
		for n < len(oct) && n < 3 && oct[n] >= '0' && oct[n] <= '7' {
			n++
		}

		v, err := strconv.ParseUint(oct[:n], 8, 32)
		if err != nil {
			return 0, 0, false
		}

		return rune(v), 1 + n, true
	}

	// Unicode: \uNNNN (4 hex) or \UNNNNNNNN (8 hex).
	if c == 'u' || c == 'U' {
		// Count the hex digits.
		hexLen := 4
		if c == 'U' {
			hexLen = 8
		}

		if len(s) < 2+hexLen {
			return 0, 0, false
		}

		hex := s[2 : 2+hexLen]
		for i := range hex {
			if !isHexDigit(hex[i]) {
				return 0, 0, false
			}
		}

		v, err := strconv.ParseUint(hex, 16, 32)
		if err != nil {
			return 0, 0, false
		}

		return rune(v), 2 + hexLen, true
	}

	return 0, 0, false
}

// isHexDigit reports whether b is an ASCII hex digit.
func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// systemdQuote encodes s as a double-quoted systemd item, escaping backslashes
// and double quotes (the two characters that are meaningful inside double
// quotes). Whitespace is left as-is because it is inside the quotes; the quotes
// are what protect it from being treated as an item separator.
func systemdQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}

	b.WriteByte('"')

	return b.String()
}

// nodeConfigArgsKey is the Environment item key the kubelet node-config drop-in
// uses for the node-level kubelet arguments.
const nodeConfigArgsKey = "KUBELET_NODE_CONFIG_ARGS"

// nodeConfigArgsItem returns the value of the EFFECTIVE KUBELET_NODE_CONFIG_ARGS
// item in a single Environment= directive, or "" when the item is absent. In
// systemd, a repeated assignment to the same variable in the same directive
// uses the LAST value, so this returns the last matching item. It decodes the
// item's quoting/escaping, so a whole-assignment-quoted item
// ("KUBELET_NODE_CONFIG_ARGS=<args>"), a value-quoted item
// (KUBELET_NODE_CONFIG_ARGS="<args>"), and a bare item all yield the same args
// string. When the item's value is quoted only partially
// (KUBELET_NODE_CONFIG_ARGS="<a>" UNRELATED=keep), only the KUBELET item's value
// is returned and the unrelated item is ignored.
func nodeConfigArgsItem(value string) (string, bool) {
	// Walk all items; the last KUBELET_NODE_CONFIG_ARGS assignment is effective.
	var (
		effective string
		found     bool
	)

	for _, item := range systemdEnvItems(value) {
		if strings.HasPrefix(item, nodeConfigArgsKey+"=") {
			effective = item[len(nodeConfigArgsKey)+1:]
			found = true
		}
	}

	return effective, found
}

// nodeConfigArgsLine reports whether line is the systemd Environment=
// directive that carries KUBELET_NODE_CONFIG_ARGS. It decodes the item's
// quoting, so the generated whole-assignment-quoted form
// Environment="KUBELET_NODE_CONFIG_ARGS=...", a value-quoted form
// Environment=KUBELET_NODE_CONFIG_ARGS="...", a bare assignment, and a directive
// that also carries other items are all recognized.
func nodeConfigArgsLine(line string) bool {
	rest := strings.TrimPrefix(line, "Environment=")
	if rest == line {
		return false
	}

	_, ok := nodeConfigArgsItem(rest)

	return ok
}

// nodeConfigArgsValue extracts the value of the EFFECTIVE KUBELET_NODE_CONFIG_ARGS
// Environment entry from the node-config drop-in. In systemd, Environment=
// accumulates entries: a later assignment of the same variable is the effective
// one, and a bare Environment= (with no items) RESETS the entire environment,
// clearing all previously-set variables. So when the drop-in assigns
// KUBELET_NODE_CONFIG_ARGS more than once the LAST assignment is returned, and
// if a reset appears after the last assignment the variable is no longer defined
// (the function returns an error). It returns the decoded args string (the value
// after the KUBELET_NODE_CONFIG_ARGS= prefix, with the item's quotes and
// escapes resolved).
func nodeConfigArgsValue(data string) (string, error) {
	// Walk lines in order; track whether a reset has occurred.
	value, found := "", false

	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)

		if !strings.HasPrefix(line, "Environment=") {
			continue
		}

		rest := strings.TrimPrefix(line, "Environment=")

		// A bare Environment= (empty value) resets the environment.
		if strings.TrimSpace(rest) == "" {
			value, found = "", false

			continue
		}

		v, ok := nodeConfigArgsItem(rest)
		if !ok {
			continue
		}

		value, found = v, true
	}

	if !found {
		return "", fmt.Errorf("no KUBELET_NODE_CONFIG_ARGS in kubelet node-config drop-in")
	}

	return value, nil
}

// nodeConfigArgs is the parsed token list of the node-config args value plus
// which tokens are the managed provider flags.
type nodeConfigArgs struct {
	tokens  []string
	managed []bool // parallel to tokens: true for a managed provider flag
}

func registryAuthProviderArgs(value string) nodeConfigArgs {
	a := nodeConfigArgs{}

	for _, tok := range strings.Fields(value) {
		a.tokens = append(a.tokens, tok)
		a.managed = append(a.managed, false)
	}

	// Mark the tokens that establish the managed flags, in both the inline
	// --flag=value and the split --flag value forms, so removeManaged can target
	// them on detach.
	for _, flag := range registryAuthProviderFlags() {
		a.markManagedEffective(providerFlagName(flag), providerFlagValue(flag))
	}

	return a
}

// providerFlagNames are the known kubelet image credential provider flag names
// (without values), in the exact spelling kubelet accepts. The managed provider
// is set with these two flags; any other provider is configured with one or
// both of them pointing elsewhere.
var providerFlagNames = []string{
	"--image-credential-provider-config",
	"--image-credential-provider-bin-dir",
}

// isProviderFlagName reports whether name is a known image credential provider
// flag name (the two the kubelet accepts).
func isProviderFlagName(name string) bool {
	for _, n := range providerFlagNames {
		if name == n {
			return true
		}
	}

	return false
}

// providerFlagName is the flag-name part of a provider flag token, or "" when
// the token is not a provider flag. It recognizes both the inline form
// --flag=value and the split form --flag (the value is the following token).
// It is what tells one provider flag apart from another: two tokens with the
// same name are the same flag, possibly carrying different values.
func providerFlagName(token string) string {
	if strings.HasPrefix(token, "--image-credential-provider-") {
		// Split form: --flag with the value as the next token.
		if !strings.Contains(token, "=") {
			if isProviderFlagName(token) {
				return token
			}

			return ""
		}

		// Inline form: --flag=value.
		name := token[:strings.IndexByte(token, '=')]

		return name
	}

	return ""
}

// providerFlagValue is the value part of a provider flag token, or "" when the
// token is a split-form flag (whose value is the following token). For the
// inline form --flag=value it returns everything after the first '='.
func providerFlagValue(token string) string {
	name := providerFlagName(token)
	if name == "" || !strings.Contains(token, "=") {
		return ""
	}

	return token[strings.IndexByte(token, '=')+1:]
}

// providerFlagEffectiveValue scans a token list and returns the effective (last
// occurrence) value of the named provider flag, handling both the inline
// --flag=value form and the split --flag value form, and whether the flag was
// present at all. The last occurrence is the effective one because kubelet
// resolves a repeated flag by the last occurrence. In the split form the value
// is the following token, which is consumed (not itself a flag).
func providerFlagEffectiveValue(tokens []string, name string) (string, bool) {
	last, found := "", false

	for i, tok := range tokens {
		if providerFlagName(tok) != name {
			continue
		}

		found = true

		if strings.Contains(tok, "=") {
			last = providerFlagValue(tok)
		} else if i+1 < len(tokens) {
			last = tokens[i+1]
		}
	}

	return last, found
}

// addManaged adds the two managed provider flags if any are missing. It refuses
// to add a flag whose name is already set (inline or split) to a different
// value (so an existing provider configured elsewhere is not silently
// overridden), while still completing a partial managed pair (a config flag
// without its bin-dir, or vice versa) so an interrupted write is repaired rather
// than refused. The effective value of each flag (its last occurrence, both
// forms honored) is what decides: equal to the managed value means the managed
// provider is already in effect for that flag; present but different means a
// conflict; absent means it must be added.
func (a *nodeConfigArgs) addManaged() (bool, error) {
	flags := registryAuthProviderFlags()

	// Mark the tokens that carry the managed flags so removeManaged can find
	// them later. A managed flag is present when its effective value equals the
	// managed value; the tokens that establish it (inline, or the split flag
	// and its value) are marked managed.
	a.managed = make([]bool, len(a.tokens))

	for _, flag := range flags {
		wantName := providerFlagName(flag)
		wantValue := providerFlagValue(flag)
		got, present := providerFlagEffectiveValue(a.tokens, wantName)

		switch {
		case !present:
			// Absent: must be added (below).
		case got == wantValue:
			// The managed value is already effective for this flag; mark the
			// token(s) that establish it so removal can target them.
			a.markManagedEffective(wantName, wantValue)
		default:
			// Present but a different value: an existing provider is in effect,
			// so the managed provider cannot be added without overriding it.
			return false, fmt.Errorf("kubelet node config already sets %q to a different value (%q); the managed provider cannot be added", wantName, got)
		}
	}

	changed := false

	for _, flag := range flags {
		wantName := providerFlagName(flag)
		wantValue := providerFlagValue(flag)

		_, present := providerFlagEffectiveValue(a.tokens, wantName)
		if present && providerFlagEffectiveValueEquals(a.tokens, wantName, wantValue) {
			continue
		}

		// Absent (or not the managed value): append the managed token.
		a.tokens = append(a.tokens, flag)
		a.managed = append(a.managed, true)
		changed = true
	}

	return changed, nil
}

// providerFlagEffectiveValueEquals reports whether the effective (last) value
// of a provider flag in a token list equals wantValue.
func providerFlagEffectiveValueEquals(tokens []string, name, wantValue string) bool {
	got, found := providerFlagEffectiveValue(tokens, name)

	return found && got == wantValue
}

// markManagedEffective marks the tokens that establish a provider flag whose
// value equals wantValue, so removeManaged can target them on detach. It marks
// every occurrence of the flag whose value equals wantValue (inline: the flag
// token; split: the flag token and its following value token), not just the
// last, so a duplicate managed token is also removed cleanly.
func (a *nodeConfigArgs) markManagedEffective(name, wantValue string) {
	for i, tok := range a.tokens {
		if providerFlagName(tok) != name {
			continue
		}

		if strings.Contains(tok, "=") {
			if providerFlagValue(tok) == wantValue {
				a.managed[i] = true
			}
		} else if i+1 < len(a.tokens) && a.tokens[i+1] == wantValue {
			a.managed[i] = true
			a.managed[i+1] = true
		}
	}
}

// removeManaged drops every managed provider flag. It returns false when there
// was nothing managed to remove (a non-managed provider is left untouched). It
// rebuilds the token list rather than deleting in place, so the removal cannot
// index past the end of a shrinking slice.
func (a *nodeConfigArgs) removeManaged() bool {
	changed := false

	keptTokens := make([]string, 0, len(a.tokens))
	keptManaged := make([]bool, 0, len(a.tokens))

	for i, tok := range a.tokens {
		if a.managed[i] {
			changed = true
			continue
		}

		keptTokens = append(keptTokens, tok)
		keptManaged = append(keptManaged, false)
	}

	a.tokens = keptTokens
	a.managed = keptManaged

	return changed
}

// nodeConfigArgsRendered rewrites only the KUBELET_NODE_CONFIG_ARGS item of the
// LAST effective Environment= directive in the original drop-in, preserving
// every other byte (the header comment, the [Service] stanza, any other
// Environment= directives, and the trailing newline) so the provider tokens are
// the only thing that changes. The item is re-encoded with systemd double-quote
// escaping (systemdQuote), so a value that round-tripped through a quoted drop-in
// (for example one containing a backslash) is written back in the form systemd
// will read as the same value. When the drop-in assigns KUBELET_NODE_CONFIG_ARGS
// more than once, only the LAST (effective) assignment is rewritten; earlier
// (overridden) assignments are preserved untouched. Any sibling items on the
// effective line (for example an unrelated Environment item) are preserved.
func nodeConfigArgsRendered(data string, a nodeConfigArgs) string {
	joined := strings.Join(a.tokens, " ")

	lines := strings.Split(data, "\n")

	lastEffective := -1

	for i, line := range lines {
		if nodeConfigArgsLine(line) {
			lastEffective = i
		}
	}

	if lastEffective < 0 {
		return data
	}

	lines[lastEffective] = rewriteKubeletItem(lines[lastEffective], joined)

	return strings.Join(lines, "\n")
}

// kubeletItemSpan finds the byte span [start, end) of the KUBELET_NODE_CONFIG_ARGS
// item within the value of an Environment= directive (the part after
// "Environment="). It returns the span that covers exactly that item, so the
// caller can replace it while leaving every sibling item byte untouched. A
// directive without the item returns ok=false.
func kubeletItemSpan(value string) (start, end int, ok bool) {
	itemStart := -1
	inQuote := byte(0)

	flush := func(i int) {
		if itemStart < 0 {
			return
		}

		if decodedItemStartsWithKey(value[itemStart:i]) {
			start, end = itemStart, i
			ok = true
		}

		itemStart = -1
	}

	for i := 0; i < len(value); i++ {
		c := value[i]

		switch {
		case inQuote != 0:
			if inQuote == '"' && c == '\\' && i+1 < len(value) {
				i++

				continue
			}

			if c == inQuote {
				inQuote = 0
			}

		case c == '"' || c == '\'':
			inQuote = c

			if itemStart < 0 {
				itemStart = i
			}

		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush(i)

			inQuote = 0

		default:
			if itemStart < 0 {
				itemStart = i
			}
		}
	}

	flush(len(value))

	return start, end, ok
}

// decodedItemStartsWithKey reports whether a raw item span decodes to text that
// begins with the KUBELET_NODE_CONFIG_ARGS key.
func decodedItemStartsWithKey(raw string) bool {
	return strings.HasPrefix(decodeOneItem(raw), nodeConfigArgsKey+"=")
}

// decodeOneItem decodes a single raw item span (which may be a quoted or
// unquoted run, or a key followed by a quoted value) into its decoded text.
func decodeOneItem(raw string) string {
	var b strings.Builder

	inQuote := byte(0)

	for i := 0; i < len(raw); i++ {
		c := raw[i]

		switch {
		case inQuote != 0:
			if inQuote == '"' && c == '\\' && i+1 < len(raw) {
				decoded, consumed, ok := systemdDecodeEscape(raw[i:])
				if ok {
					b.WriteRune(decoded)

					i += consumed - 1

					continue
				}

				b.WriteByte(c)

				continue
			}

			if c == inQuote {
				inQuote = 0

				continue
			}

			b.WriteByte(c)

		case c == '"' || c == '\'':
			inQuote = c

		default:
			b.WriteByte(c)
		}
	}

	return b.String()
}

// rewriteKubeletItem re-encodes the KUBELET_NODE_CONFIG_ARGS item of a single
// Environment= directive, replacing only that item's span with
// KUBELET_NODE_CONFIG_ARGS=<quoted args> and leaving every sibling item's bytes
// untouched. The args are re-encoded with systemd double-quote escaping
// (systemdQuote), so a value that round-tripped through a quoted drop-in (for
// example one containing a backslash) is written back as the value systemd
// reads it as.
func rewriteKubeletItem(line, args string) string {
	rest := strings.TrimPrefix(line, "Environment=")
	carriage := strings.HasSuffix(rest, "\r")

	if carriage {
		rest = strings.TrimSuffix(rest, "\r")
	}

	start, end, ok := kubeletItemSpan(rest)
	if !ok {
		return line
	}

	replacement := nodeConfigArgsKey + "=" + systemdQuote(args)

	var b strings.Builder
	b.WriteString(rest[:start])
	b.WriteString(replacement)
	b.WriteString(rest[end:])

	suffix := ""

	if carriage {
		suffix = "\r"
	}

	return "Environment=" + b.String() + suffix
}

// registryAuthBackoff returns the delay to wait before the next restart attempt
// given the number of attempts already charged. It is the documented schedule:
// base for the first attempt, doubling thereafter, capped at the maximum.
func registryAuthBackoff(attempts int) time.Duration {
	backoff := registryAuthBaseBackoff

	for i := 0; i < attempts; i++ {
		backoff *= registryAuthBackoffFactor
		if backoff > registryAuthMaxBackoff {
			backoff = registryAuthMaxBackoff
		}
	}

	return backoff
}

// registryAuthPolicyHash is a stable digest of the active policy, used to tell
// whether the bounded-restart budget applies to the same policy as last time.
func registryAuthPolicyHash(policy registryauth.Config) string {
	data, err := json.Marshal(policy)
	if err != nil {
		// The policy is a struct of plain strings; marshaling cannot fail in
		// practice. A failure would be a programming error, but the hash is
		// only used to key the restart budget, so an empty digest is safe.
		return ""
	}

	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// registryAuthBudgetExhausted is returned when the bounded-restart budget is
// spent. The agent stops retrying and reports the failure; it does not mark the
// condition healthy.
type registryAuthBudgetExhausted struct {
	budget *registryAuthBudget
}

func (e *registryAuthBudgetExhausted) Error() string {
	return fmt.Sprintf("registry-auth restart budget exhausted after %d attempts", e.budget.Attempts)
}

// registryAuthBackoffPending is returned when the backoff since the last
// attempt has not elapsed; the caller schedules the next attempt.
type registryAuthBackoffPending struct {
	wait time.Duration
}

func (b *registryAuthBackoffPending) Error() string {
	return fmt.Sprintf("registry-auth restart backoff pending for %s", b.wait)
}

// registryAuthFailureOutcome maps a bounded-restart error to the condition
// reason, message, and requeue delay.
func registryAuthFailureOutcome(_ string, err error) (reason, message string, after time.Duration) {
	switch {
	case isBackoffPending(err):
		return "RestartBackoff", "waiting out the bounded-restart backoff after a failed kubelet restart", backoffWait(err)
	case isBudgetExhausted(err):
		return registryauth.ConditionReasonRestartBudgetExhausted, err.Error(), 0
	default:
		return "RestartFailed", err.Error(), registryAuthRequeue
	}
}

func isBackoffPending(err error) bool {
	_, ok := err.(*registryAuthBackoffPending)

	return ok
}

func isBudgetExhausted(err error) bool {
	_, ok := err.(*registryAuthBudgetExhausted)

	return ok
}

func backoffWait(err error) time.Duration {
	if b, ok := err.(*registryAuthBackoffPending); ok {
		return b.wait
	}

	return 0
}

// loadRegistryAuthBudget reads the persisted budget for a slot. A missing file
// yields a fresh budget. A malformed file is an error, not a fresh budget: an
// interrupted write must not restore all restart attempts and contradict the
// persistence-across-restarts guarantee (finding #7).
func loadRegistryAuthBudget(slot string) (*registryAuthBudget, error) {
	path := registryAuthBudgetPath(slot)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &registryAuthBudget{}, nil
		}

		return nil, fmt.Errorf("read registry-auth budget for %s: %w", slot, err)
	}

	var budget registryAuthBudget
	if err := json.Unmarshal(data, &budget); err != nil {
		return nil, fmt.Errorf("corrupt registry-auth budget for %s: %w", slot, err)
	}

	return &budget, nil
}

// resetBudget clears a slot's bounded-restart budget, keeping the policy hash so
// the budget stays tied to the currently verified policy. It is called when the
// policy is verified healthy, so a healthy node does not carry a stale attempt
// count.
func (r *registryAuthReconciler) resetBudget(slot, policyHash string) error {
	return saveRegistryAuthBudget(slot, &registryAuthBudget{PolicyHash: policyHash})
}

// saveRegistryAuthBudget persists the budget for a slot atomically (write a
// temp file in the same directory and rename it into place), so an interrupted
// write cannot leave a corrupt budget that loadRegistryAuthBudget would then
// treat as an error or, worse, silently reset.
func saveRegistryAuthBudget(slot string, budget *registryAuthBudget) error {
	path := registryAuthBudgetPath(slot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(budget)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".registry-auth-budget-*")
	if err != nil {
		return fmt.Errorf("create temp budget for %s: %w", slot, err)
	}

	name := tmp.Name()
	defer os.Remove(name) //nolint:errcheck // best-effort cleanup of the temp file

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup; the write error is the one to report

		return fmt.Errorf("write registry-auth budget for %s: %w", slot, err)
	}

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup; the chmod error is the one to report

		return fmt.Errorf("chmod registry-auth budget for %s: %w", slot, err)
	}

	// Sync the content before the rename so the budget data is durable.
	if err := fsyncFile(tmp); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup; the sync error is the one to report

		return fmt.Errorf("fsync registry-auth budget for %s: %w", slot, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close registry-auth budget for %s: %w", slot, err)
	}

	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("persist registry-auth budget for %s: %w", slot, err)
	}

	// Sync the destination directory after the rename so the final entry is
	// durable, not just the temporary name.
	return fsyncDir(path)
}
