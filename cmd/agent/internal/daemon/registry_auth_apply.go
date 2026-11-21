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

// registryAuthManagedMarker prefixes the agent's registry-auth drop-in so the
// agent (and a future detach) can recognize and remove only the files it owns.
const registryAuthManagedMarker = "# Managed by unbounded-agent registry authentication.\n"

// registryAuthDropInName is the drop-in file name the agent writes into the
// kubelet unit's drop-in directory.
const registryAuthDropInName = "90-unbounded-registry-auth.conf"

// registryAuthRoot re-resolves the node-stack root for the active machine's
// slot so the reconciler can place provider files by in-node path. It resolves
// the goal state for the applied configuration because the registry-auth files
// are placed next to the running stack, not as part of a repave. The preflight
// resolver is used so the placement does not require device readiness. The
// provider binary is a fixed upstream release, so no download override is
// needed here; the default source is used.
func registryAuthRoot(log *slog.Logger, active *ActiveMachine) (isolation.NodeRoot, error) {
	gs, err := goalstates.ResolveMachineForPreflight(log, active.Config, active.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve node stack root for %s: %w", active.Name, err)
	}

	return gs.NodeStart.Root(), nil
}

// ensureACRProviderBinary downloads the stock ACR credential provider into the
// node stack if it is not already present. It reuses the rootfs download task so
// the binary lands and is verified exactly as it would during provisioning.
func ensureACRProviderBinary(ctx context.Context, log *slog.Logger, active *ActiveMachine, root isolation.NodeRoot) error {
	gs, err := goalstates.ResolveMachineForPreflight(log, active.Config, active.Name, nil)
	if err != nil {
		return fmt.Errorf("resolve node stack for ACR provider: %w", err)
	}

	// The download task writes through the node stack root. Point it at the
	// resolved root so it places the binary where the running stack expects it.
	gs.RootFS.RootOverride = root

	return rootfs.DownloadACRProvider(log, gs.RootFS).Do(ctx)
}

// registryAuthKubeletUnitFile is the kubelet unit file for the mode, as seen
// through the node-stack root.
func registryAuthKubeletUnitFile(mode isolation.Mode) string {
	switch mode {
	case isolation.ModeNone:
		return filepath.Join(goalstates.SystemdSystemDir, "unbounded-kubelet.service")
	default:
		return filepath.Join(goalstates.SystemdSystemDir, goalstates.SystemdUnitKubelet)
	}
}

// registryAuthDropInPath is the path (through the node-stack root) of the
// registry-auth drop-in for the mode.
func registryAuthDropInPath(root isolation.NodeRoot, mode isolation.Mode) string {
	unitDir := filepath.Dir(registryAuthKubeletUnitFile(mode)) + ".d"

	return root.Path(filepath.Join(unitDir, registryAuthDropInName))
}

// kubeletExecStartFromUnit reads the kubelet unit's ExecStart line through the
// node-stack root. The registry-auth drop-in must restate this line (systemd
// replaces the unit's ExecStart with the drop-in's) and append the managed
// flags.
func kubeletExecStartFromUnit(root isolation.NodeRoot, mode isolation.Mode) (string, error) {
	path := root.Path(registryAuthKubeletUnitFile(mode))

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read kubelet unit %s: %w", path, err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExecStart=") {
			return strings.TrimPrefix(line, "ExecStart="), nil
		}
	}

	return "", fmt.Errorf("no ExecStart line in kubelet unit %s", path)
}

// writeRegistryAuthDropInFor writes the drop-in that sets the image credential
// provider flags for the mode, or removes it when policy is nil. It reports
// whether the on-disk content changed.
func writeRegistryAuthDropInFor(root isolation.NodeRoot, mode isolation.Mode, policy *registryauth.Config) (bool, error) {
	path := registryAuthDropInPath(root, mode)

	if policy == nil {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}

		return false, nil
	}

	execStart, err := kubeletExecStartFromUnit(root, mode)
	if err != nil {
		return false, err
	}

	if !strings.Contains(execStart, "$KUBELET_NODE_CONFIG_ARGS") {
		return false, fmt.Errorf("kubelet ExecStart %q does not reference the node config args; refusing to override it", execStart)
	}
	if strings.Contains(execStart, "--image-credential-provider-") {
		return false, fmt.Errorf("kubelet ExecStart already sets image credential provider flags; the managed provider cannot be added")
	}

	data := registryAuthDropInContent(execStart)

	return writeFileIfChanged(path, []byte(data), 0o644)
}

// registryAuthDropInContent renders the [Service] drop-in restating the kubelet
// ExecStart with the managed image credential provider flags appended.
func registryAuthDropInContent(execStart string) string {
	return registryAuthManagedMarker +
		"[Service]\n" +
		"ExecStart=\n" +
		"ExecStart=" + execStart +
		" --image-credential-provider-config=" + goalstates.RegistryAuthCredentialConfigPath +
		" --image-credential-provider-bin-dir=" + goalstates.RegistryAuthProviderBinDir + "\n"
}

// removeRegistryAuthDropIn removes the managed drop-in from both the nspawn and
// none kubelet unit locations, leaving the kubelet's authentication untouched.
// It reports whether the content it removed was the agent's own (so the caller
// knows a restart is needed).
func removeRegistryAuthDropIn(root isolation.NodeRoot) (bool, error) {
	changed := false

	for _, unitDir := range []string{
		filepath.Join(goalstates.SystemdSystemDir, "kubelet.service.d"),
		filepath.Join(goalstates.SystemdSystemDir, "unbounded-kubelet.service.d"),
	} {
		path := root.Path(filepath.Join(unitDir, registryAuthDropInName))
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return changed, err
		}

		// Only remove a drop-in the agent owns, so an unrelated drop-in at the
		// same name is never touched.
		data, err := os.ReadFile(path)
		if err != nil {
			return changed, err
		}

		if !strings.HasPrefix(string(data), registryAuthManagedMarker) {
			continue
		}

		if err := os.Remove(path); err != nil {
			return changed, err
		}

		changed = true
	}

	return changed, nil
}

// registryAuthBackoff returns the backoff delay after the given number of
// charged attempts.
func registryAuthBackoff(attempts int) time.Duration {
	backoff := registryAuthBaseBackoff
	for i := 1; i < attempts; i++ {
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

// loadRegistryAuthBudget reads the persisted budget for a slot. A missing or
// unreadable file yields a fresh budget.
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
		// A corrupt budget is treated as fresh rather than blocking the node.
		return &registryAuthBudget{}, nil
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

// saveRegistryAuthBudget persists the budget for a slot.
func saveRegistryAuthBudget(slot string, budget *registryAuthBudget) error {
	path := registryAuthBudgetPath(slot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(budget)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write registry-auth budget for %s: %w", slot, err)
	}

	return nil
}
