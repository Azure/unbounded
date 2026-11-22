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
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

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
// The offline-artifact bundle is intentionally NOT resolved here: resolution
// fetches and verifies the entire bundle, which would force a bootstrap source
// to remain available for every reconcile pass. The provider download
// (ensureACRProviderBinary) resolves the bundle lazily, only when a download is
// actually needed.
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
// offline mirror) rather than always the default upstream URL.
//
// The offline bundle is resolved only when a download is actually needed. A node
// that already carries the provider binary (the common steady state) does not
// need the bootstrap source to remain available.
func ensureACRProviderBinary(ctx context.Context, log *slog.Logger, active *ActiveMachine, gs *goalstates.MachineGoalState) error {
	gs.RootFS.RootOverride = gs.NodeStart.Root()

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
// kubelet node-config drop-in. It is the mode-correct, mode-aware path the
// kubelet actually reads.
func registryAuthNodeConfigDropIn(root isolation.NodeRoot, mode isolation.Mode) string {
	return root.Path(filepath.Join(goalstates.SystemdSystemDir, registryAuthKubeletUnitName(mode)+".d", "20-node-config.conf"))
}

// registryAuthProviderFlags are the kubelet image credential provider flags the
// agent toggles in the node-config drop-in.
func registryAuthProviderFlags() []string {
	return []string{
		"--image-credential-provider-config=" + goalstates.RegistryAuthCredentialConfigPath,
		"--image-credential-provider-bin-dir=" + goalstates.RegistryAuthProviderBinDir,
	}
}

// applyProviderToNodeConfig re-renders the kubelet node-config drop-in from the
// same template the nodestart phase uses, with the provider flags on (policy
// non-nil) or off (policy nil). It reports whether the on-disk content changed,
// which is what triggers a kubelet restart.
//
// The drop-in is written by the unbounded agent at provisioning time from
// assets/20-node-config.conf. Re-rendering from the same template with the
// same node values (NodeName, NodeIP, labels, taints from the applied config)
// produces a file that is byte-identical to the provisioning output except for
// the provider flags. This is simpler and safer than parsing the existing
// drop-in: if the file was manually edited, we re-render the canonical form,
// which is what the agent already does on every repave.
func applyProviderToNodeConfig(root isolation.NodeRoot, mode isolation.Mode, active *ActiveMachine, gs *goalstates.MachineGoalState, policy *registryauth.Config) (bool, error) {
	path := registryAuthNodeConfigDropIn(root, mode)

	// Reconcile the provider flags on the goal state with the desired policy,
	// WITHOUT clobbering a customer-configured provider that the applied
	// AgentConfig declared. The goal state's ImageCredentialProvider (projected
	// from the applied config) is the authoritative customer value.
	//
	//   - External/detach (policy nil): leave the customer provider as-is (it
	//     may be nil). Do NOT clear it: a customer provider declared via the
	//     supported config must survive registry-auth detach.
	//   - Managed/attach (policy non-nil): if the customer configured a DIFFERENT
	//     provider, reject the conflict before writing anything. If none (or the
	//     managed pair itself), set the managed pair.
	switch {
	case policy == nil:
		// Customer provider (or none) stays as projected from the applied config.
		// No mutation: gs.NodeStart.Kubelet.ImageCredentialProvider is already
		// whatever the customer configured (nil or their paths).
	case gs.NodeStart.Kubelet.ImageCredentialProvider == nil:
		// No customer provider: apply the managed pair.
		gs.NodeStart.Kubelet.ImageCredentialProvider = managedRegistryAuthProvider()
	default:
		// Customer configured a provider. Reject if it is not the managed pair,
		// so we never silently overwrite a customer's registry auth.
		cust := gs.NodeStart.Kubelet.ImageCredentialProvider
		if cust.ConfigPath != goalstates.RegistryAuthCredentialConfigPath || cust.BinDir != goalstates.RegistryAuthProviderBinDir {
			return false, fmt.Errorf(
				"registry-auth: node config already declares image-credential-provider "+
					"(config=%s bin-dir=%s); cannot attach the managed provider without first removing the customer provider "+
					"from the agent config; refusing to overwrite customer registry auth",
				cust.ConfigPath, cust.BinDir)
		}
	}

	rendered, err := nodestart.RenderNodeConfigDropIn(&gs.NodeStart.Kubelet, gs.NodeStart.NodeName, gs.NodeStart.Kubelet.NodeIP)
	if err != nil {
		return false, fmt.Errorf("render node-config drop-in: %w", err)
	}

	return writeFileIfChanged(path, rendered, 0o644)
}

// managedRegistryAuthProvider returns the managed provider pair (the credential
// config path and bin dir the agent writes for registry-auth).
func managedRegistryAuthProvider() *goalstates.ImageCredentialProvider {
	return &goalstates.ImageCredentialProvider{
		ConfigPath: goalstates.RegistryAuthCredentialConfigPath,
		BinDir:     goalstates.RegistryAuthProviderBinDir,
	}
}

// executableFile reports whether path exists, is a regular file, and has any
// execute bit set.
func executableFile(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}

	return !s.IsDir() && s.Mode()&0o111 != 0
}

// writeFileIfChanged atomically writes content to path and reports whether the
// on-disk bytes differed.
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
		defer os.Remove(name) //nolint:errcheck // best-effort cleanup

		if _, err := tmp.Write(content); err != nil {
			tmp.Close() //nolint:errcheck // best-effort cleanup

			return false, err
		}

		if err := tmp.Chmod(perm); err != nil {
			tmp.Close() //nolint:errcheck // best-effort cleanup

			return false, err
		}

		if err := fsyncFile(tmp); err != nil {
			tmp.Close() //nolint:errcheck // best-effort cleanup

			return false, err
		}

		if err := tmp.Close(); err != nil {
			return false, err
		}

		if err := os.Rename(name, path); err != nil {
			return false, err
		}

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

// fsyncFile syncs the contents of an open file to durable storage.
func fsyncFile(f *os.File) error {
	return f.Sync()
}

// fsyncDir syncs the directory entry for path, so a rename is durable across a
// power loss.
func fsyncDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}

	defer d.Close() //nolint:errcheck

	return d.Sync()
}

// writeJSONFile atomically writes a JSON value to path.
func writeJSONFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".registry-auth-*")
	if err != nil {
		return err
	}

	name := tmp.Name()
	defer os.Remove(name) //nolint:errcheck // best-effort cleanup

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup

		return err
	}

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup

		return err
	}

	if err := fsyncFile(tmp); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(name, path); err != nil {
		return err
	}

	return fsyncDir(path)
}

// registryAuthBackoff returns the delay to wait before the next restart attempt
// given the number of attempts already charged.
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

// registryAuthPolicyHash is a stable digest of the active policy.
func registryAuthPolicyHash(policy registryauth.Config) string {
	data, err := json.Marshal(policy)
	if err != nil {
		return ""
	}

	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// registryAuthBudgetExhausted is returned when the bounded-restart budget is
// spent.
type registryAuthBudgetExhausted struct {
	budget *registryAuthBudget
}

func (e *registryAuthBudgetExhausted) Error() string {
	return fmt.Sprintf("registry-auth restart budget exhausted after %d attempts", e.budget.Attempts)
}

// registryAuthBackoffPending is returned when the backoff since the last
// attempt has not elapsed.
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

// loadRegistryAuthBudget reads the persisted budget for a slot.
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

// resetBudget clears a slot's bounded-restart budget.
func (r *registryAuthReconciler) resetBudget(slot, policyHash string) error {
	return saveRegistryAuthBudget(slot, &registryAuthBudget{PolicyHash: policyHash})
}

// saveRegistryAuthBudget persists the budget for a slot atomically.
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
	defer os.Remove(name) //nolint:errcheck // best-effort cleanup

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup

		return fmt.Errorf("write registry-auth budget for %s: %w", slot, err)
	}

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup

		return fmt.Errorf("chmod registry-auth budget for %s: %w", slot, err)
	}

	if err := fsyncFile(tmp); err != nil {
		tmp.Close() //nolint:errcheck // best-effort cleanup

		return fmt.Errorf("fsync registry-auth budget for %s: %w", slot, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close registry-auth budget for %s: %w", slot, err)
	}

	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("persist registry-auth budget for %s: %w", slot, err)
	}

	return fsyncDir(path)
}

// registryAuthArgvHasManagedFlags reports whether the argv carries the managed
// provider in effect. It recognizes BOTH the inline form (--flag=value) and the
// split form (--flag value), because kubelet's argv may be assembled from
// KUBELET_EXTRA_ARGS or other sources that split the flag and its value into
// separate tokens. A bare exact-token match would miss the split form and
// falsely conclude the managed provider was removed.
//
// The managed provider is "in effect" when at least one managed flag name is
// present and its effective (last-occurrence) value equals the managed value, so
// a single remaining managed flag (config or bin-dir) is detected even when the
// other was already cleared, and repeated-flag semantics are honored.
func registryAuthArgvHasManagedFlags(argv []string) bool {
	for _, managed := range registryAuthProviderFlags() {
		name := providerFlagName(managed)
		if name == "" {
			continue
		}

		wantValue := providerFlagValue(managed)
		got, found := providerFlagEffectiveValue(argv, name)

		if found && got == wantValue {
			return true
		}
	}

	return false
}

// registryAuthEffectiveManagedFlags reports whether the argv's effective image
// credential provider configuration is the managed one: for each managed flag,
// the flag name is present and its last occurrence equals the managed value.
func registryAuthEffectiveManagedFlags(argv []string) bool {
	for _, managed := range registryAuthProviderFlags() {
		name := providerFlagName(managed)
		if name == "" {
			return false
		}

		wantValue := providerFlagValue(managed)
		got, found := providerFlagEffectiveValue(argv, name)

		if !found || got != wantValue {
			return false
		}
	}

	return true
}

// providerFlagName is the flag-name part of a provider flag token, or "" when
// the token is not a provider flag.
func providerFlagName(token string) string {
	if strings.HasPrefix(token, "--image-credential-provider-") {
		if !strings.Contains(token, "=") {
			return token
		}

		return token[:strings.IndexByte(token, '=')]
	}

	return ""
}

// providerFlagValue is the value part of a provider flag token.
func providerFlagValue(token string) string {
	name := providerFlagName(token)
	if name == "" || !strings.Contains(token, "=") {
		return ""
	}

	return token[strings.IndexByte(token, '=')+1:]
}

// providerFlagEffectiveValue scans a token list and returns the effective (last
// occurrence) value of the named provider flag.
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

// registryAuthProcessArgv reads /proc/<pid>/cmdline in the node's namespace.
func registryAuthProcessArgv(ctx context.Context, backend isolation.Backend, active *ActiveMachine, pid int) ([]string, error) {
	res, err := backend.Exec(ctx, isolation.SlotName(active.Name), isolation.ExecSpec{
		Args: []string{"cat", fmt.Sprintf("/proc/%d/cmdline", pid)},
	})
	if err != nil {
		return nil, err
	}

	raw := res.Stdout

	if raw == "" {
		return nil, fmt.Errorf("no argv for kubelet pid %d", pid)
	}

	fields := strings.Split(strings.TrimRight(raw, "\x00"), "\x00")

	return fields, nil
}

// registryAuthMainPID reads the unit's MainPID through the node's systemd
// manager.
func registryAuthMainPID(ctx context.Context, backend isolation.Backend, active *ActiveMachine, unit string) (int, error) {
	res, err := backend.Exec(ctx, isolation.SlotName(active.Name), isolation.ExecSpec{
		Args: []string{"systemctl", "show", "-p", "MainPID", "--value", unit},
	})
	if err != nil {
		return 0, err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		return 0, fmt.Errorf("parse MainPID %q: %w", strings.TrimSpace(res.Stdout), err)
	}

	return pid, nil
}
