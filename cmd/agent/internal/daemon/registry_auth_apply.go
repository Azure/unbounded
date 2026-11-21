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
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

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
		defer os.Remove(name)

		if _, err := tmp.Write(content); err != nil {
			tmp.Close()

			return false, err
		}

		if err := tmp.Chmod(perm); err != nil {
			tmp.Close()

			return false, err
		}

		if err := tmp.Close(); err != nil {
			return false, err
		}

		if err := os.Rename(name, path); err != nil {
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
// readiness, and no disruptive Prepare is invoked just to discover a root. The
// applied config's offline-artifact download override is resolved and wired
// into the goal state so the provider-binary download honors an offline mirror
// (finding: air-gapped nspawn nodes are the motivating environment, and the
// provider is no longer supplied by an installer image).
func registryAuthNodeStart(ctx context.Context, log *slog.Logger, active *ActiveMachine) (*goalstates.MachineGoalState, error) {
	downloads, _, err := goalstates.ResolveDownloadOverridesWithOfflineArtifacts(ctx, active.Config, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve download overrides for %s: %w", active.Name, err)
	}

	gs, err := goalstates.ResolveMachineForPreflight(log, active.Config, active.Name, downloads)
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
func ensureACRProviderBinary(ctx context.Context, log *slog.Logger, active *ActiveMachine, gs *goalstates.MachineGoalState) error {
	// The download task writes through the node stack root. Point it at the
	// resolved root so it places the binary where the running stack expects it.
	gs.RootFS.RootOverride = gs.NodeStart.Root()

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

		return writeFileIfChanged(path, []byte(nodeConfigArgsRendered(envValue, args)), 0o644)
	}

	added, err := args.addManaged()
	if err != nil {
		return false, err
	}

	if !added {
		return false, nil
	}

	return writeFileIfChanged(path, []byte(nodeConfigArgsRendered(envValue, args)), 0o644)
}

// nodeConfigArgsValue extracts the value of the KUBELET_NODE_CONFIG_ARGS
// Environment entry from the node-config drop-in.
func nodeConfigArgsValue(data string) (string, error) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimPrefix(line, "Environment=")
		if !strings.HasPrefix(line, "KUBELET_NODE_CONFIG_ARGS=") {
			continue
		}

		value := strings.TrimPrefix(line, "KUBELET_NODE_CONFIG_ARGS=")

		// The value is a systemd Environment= assignment, quoted.
		if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			value = value[1 : len(value)-1]
		}

		return value, nil
	}

	return "", fmt.Errorf("no KUBELET_NODE_CONFIG_ARGS in kubelet node-config drop-in")
}

// nodeConfigArgs is the parsed token list of the node-config args value plus
// which tokens are the managed provider flags.
type nodeConfigArgs struct {
	tokens  []string
	managed []bool // parallel to tokens: true for a managed provider flag
}

func registryAuthProviderArgs(value string) nodeConfigArgs {
	managedSet := map[string]bool{}
	for _, f := range registryAuthProviderFlags() {
		managedSet[f] = true
	}

	a := nodeConfigArgs{}
	for _, tok := range strings.Fields(value) {
		a.tokens = append(a.tokens, tok)
		a.managed = append(a.managed, managedSet[tok])
	}

	return a
}

// addManaged adds the two managed provider flags if any are missing. It refuses
// to add a flag whose path differs from the managed value (so an existing
// provider configured elsewhere is not silently overridden).
func (a *nodeConfigArgs) addManaged() (bool, error) {
	flags := registryAuthProviderFlags()

	for _, flag := range flags {
		present := false
		seenOther := false

		for i, tok := range a.tokens {
			if tok == flag {
				present = true
				a.managed[i] = true
			} else if strings.HasPrefix(tok, "--image-credential-provider-") {
				seenOther = true
			}
		}

		if seenOther && !present {
			return false, fmt.Errorf("kubelet node config already sets a different %q; the managed provider cannot be added", flag)
		}
	}

	changed := false

	for _, flag := range flags {
		found := false
		for _, tok := range a.tokens {
			if tok == flag {
				found = true
			}
		}

		if !found {
			a.tokens = append(a.tokens, flag)
			a.managed = append(a.managed, true)
			changed = true
		}
	}

	return changed, nil
}

// removeManaged drops any managed provider flag. It returns false when there was
// nothing managed to remove (a non-managed provider is left untouched).
func (a *nodeConfigArgs) removeManaged() bool {
	changed := false

	for i := range a.tokens {
		if a.managed[i] {
			a.tokens = append(a.tokens[:i], a.tokens[i+1:]...)
			a.managed = append(a.managed[:i], a.managed[i+1:]...)
			changed = true

			i--
		}
	}

	return changed
}

// nodeConfigArgsRendered rewrites only the KUBELET_NODE_CONFIG_ARGS Environment
// entry, preserving every other byte of the drop-in (including the trailing
// newline) so the node-config args stay the only thing that changes.
func nodeConfigArgsRendered(data string, a nodeConfigArgs) string {
	joined := strings.Join(a.tokens, " ")
	replacement := "Environment=\"KUBELET_NODE_CONFIG_ARGS=" + joined + "\""

	lines := strings.Split(data, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimPrefix(line, "Environment="), "KUBELET_NODE_CONFIG_ARGS=") {
			lines[i] = replacement
		}
	}

	return strings.Join(lines, "\n")
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
	data, _ := json.Marshal(policy)

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
		return "RestartBudgetExhausted", err.Error(), 0
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
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()

		return fmt.Errorf("write registry-auth budget for %s: %w", slot, err)
	}

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()

		return fmt.Errorf("chmod registry-auth budget for %s: %w", slot, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close registry-auth budget for %s: %w", slot, err)
	}

	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("persist registry-auth budget for %s: %w", slot, err)
	}

	return nil
}
