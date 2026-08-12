// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package noderoute

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const stateVersion = 2

// Options identifies the host files managed by the standalone node reconciler.
type Options struct {
	DesiredPath             string
	HostCertsDir            string
	HostContainerdConfig    string
	HostStateDir            string
	ExpectedContainerdCerts string
}

// DefaultOptions returns paths matching a standard AKS containerd node and the
// standalone DaemonSet mounts.
func DefaultOptions() Options {
	return Options{
		DesiredPath:             "/etc/gantry-node-config/registries.json",
		HostCertsDir:            "/host-certs",
		HostContainerdConfig:    "/host-containerd-config/config.toml",
		HostStateDir:            "/host-state",
		ExpectedContainerdCerts: "/etc/containerd/certs.d",
	}
}

type routeState struct {
	Version              int    `json:"version"`
	Host                 string `json:"host"`
	OriginalPresent      bool   `json:"originalPresent"`
	OriginalMode         uint32 `json:"originalMode,omitempty"`
	OriginalSHA256       string `json:"originalSHA256,omitempty"`
	OriginalBackup       string `json:"originalBackup,omitempty"`
	ManagedSHA256        string `json:"managedSHA256"`
	PendingManagedSHA256 string `json:"pendingManagedSHA256,omitempty"`
	Applied              bool   `json:"applied"`
}

// Reconcile converges all desired registry routes and restores routes removed
// from the desired set.
func Reconcile(ctx context.Context, options Options, desired Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := validateOptions(options); err != nil {
		return err
	}

	if err := desired.Validate(); err != nil {
		return err
	}

	if len(desired.Registries) > 0 {
		if err := verifyContainerdConfig(options); err != nil {
			return err
		}

		if err := rejectCompetingDefaultRoute(options, desired); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(options.HostStateDir, 0o700); err != nil {
		return fmt.Errorf("create route state directory: %w", err)
	}

	states, err := loadStates(options.HostStateDir)
	if err != nil {
		return err
	}

	desiredHosts := make(map[string]struct{}, len(desired.Registries))
	for _, registry := range desired.Registries {
		desiredHosts[registry.Host] = struct{}{}
		if err := ensureTargetDirectory(options, registry.Host); err != nil {
			return fmt.Errorf("validate registry directory %s: %w", registry.Host, err)
		}

		if err := ensureRoute(options, registry); err != nil {
			return fmt.Errorf("configure registry %s: %w", registry.Host, err)
		}
	}

	for _, state := range states {
		if _, ok := desiredHosts[state.Host]; ok {
			continue
		}

		if err := restoreRoute(options, state); err != nil {
			return fmt.Errorf("restore registry %s: %w", state.Host, err)
		}
	}

	return nil
}

// Check verifies that host state exactly matches the desired configuration.
func Check(options Options, desired Config) error {
	if err := validateOptions(options); err != nil {
		return err
	}

	if err := desired.Validate(); err != nil {
		return err
	}

	if len(desired.Registries) > 0 {
		if err := verifyContainerdConfig(options); err != nil {
			return err
		}

		if err := rejectCompetingDefaultRoute(options, desired); err != nil {
			return err
		}
	}

	states, err := loadStates(options.HostStateDir)
	if err != nil {
		return err
	}

	statesByHost := make(map[string]routeState, len(states))
	for _, state := range states {
		statesByHost[state.Host] = state
	}

	desiredHosts := make(map[string]struct{}, len(desired.Registries))
	for _, registry := range desired.Registries {
		desiredHosts[registry.Host] = struct{}{}

		state, ok := statesByHost[registry.Host]
		if !ok {
			return fmt.Errorf("registry route %s has no ownership state", registry.Host)
		}

		managedSHA := checksum(renderHosts(registry))
		if !state.Applied || state.PendingManagedSHA256 != "" || state.ManagedSHA256 != managedSHA {
			return fmt.Errorf("registry route %s ownership state has not converged", registry.Host)
		}

		if err := ensureTargetDirectory(options, registry.Host); err != nil {
			return fmt.Errorf("validate registry directory %s: %w", registry.Host, err)
		}

		actual, mode, err := readRegularFile(targetPath(options, registry.Host))
		if err != nil {
			return fmt.Errorf("read registry route %s: %w", registry.Host, err)
		}

		if !bytes.Equal(actual, renderHosts(registry)) {
			return fmt.Errorf("registry route %s has not converged", registry.Host)
		}

		if mode.Perm() != 0o644 {
			return fmt.Errorf("registry route %s mode is %o, want 644", registry.Host, mode.Perm())
		}
	}

	for _, state := range states {
		if _, ok := desiredHosts[state.Host]; !ok {
			return fmt.Errorf("removed registry route %s has not been restored", state.Host)
		}
	}

	return nil
}

func validateOptions(options Options) error {
	if options.HostCertsDir == "" || options.HostContainerdConfig == "" || options.HostStateDir == "" || options.ExpectedContainerdCerts == "" {
		return errors.New("node-route paths must not be empty")
	}

	return nil
}

func verifyContainerdConfig(options Options) error {
	data, err := os.ReadFile(options.HostContainerdConfig)
	if err != nil {
		return fmt.Errorf("read host containerd config: %w", err)
	}

	if !containerdUsesCertsDir(data, options.ExpectedContainerdCerts) {
		return fmt.Errorf("containerd config does not set registry config_path to %q", options.ExpectedContainerdCerts)
	}

	return nil
}

func containerdUsesCertsDir(data []byte, expected string) bool {
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.SplitN(rawLine, "#", 2)[0])

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "config_path" {
			continue
		}

		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}

		if value == expected {
			return true
		}
	}

	return false
}

func rejectCompetingDefaultRoute(options Options, desired Config) error {
	if len(desired.Registries) == 0 {
		return nil
	}

	data, err := os.ReadFile(filepath.Join(options.HostCertsDir, "_default", "hosts.toml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read containerd default registry route: %w", err)
	}

	if bytes.Contains(data, []byte("127.0.0.1:5000")) {
		return errors.New("containerd _default/hosts.toml already routes to 127.0.0.1:5000; refusing to overlap another Gantry installation")
	}

	return nil
}

func ensureRoute(options Options, registry Registry) error {
	managed := renderHosts(registry)
	managedSHA := checksum(managed)
	statePath := stateFile(options.HostStateDir, registry.Host)
	state, stateErr := readState(statePath)

	target := targetPath(options, registry.Host)
	current, currentMode, currentErr := readRegularFile(target)

	currentMissing := errors.Is(currentErr, fs.ErrNotExist)
	if currentErr != nil && !currentMissing {
		return currentErr
	}

	if errors.Is(stateErr, fs.ErrNotExist) {
		managedWithoutState := !currentMissing && bytes.Equal(current, managed)
		if !currentMissing && !managedWithoutState && !registry.ReplaceExisting && !registry.ManageReplacements {
			return fmt.Errorf("unmanaged %s exists; rerun with replacement explicitly enabled", target)
		}

		state = routeState{
			Version:         stateVersion,
			Host:            registry.Host,
			OriginalPresent: !currentMissing && !managedWithoutState,
			ManagedSHA256:   managedSHA,
		}
		if state.OriginalPresent {
			state.OriginalMode = uint32(currentMode.Perm())

			state.OriginalSHA256 = checksum(current)
			if err := writeFileAtomic(originalFile(options.HostStateDir, registry.Host), current, 0o600); err != nil {
				return fmt.Errorf("back up existing route: %w", err)
			}
		}

		if err := writeJSONAtomic(statePath, state, 0o600); err != nil {
			return fmt.Errorf("record route ownership: %w", err)
		}
	} else if stateErr != nil {
		return stateErr
	} else {
		if state.Version != stateVersion || state.Host != registry.Host {
			return fmt.Errorf("invalid ownership state in %s", statePath)
		}

		originalModeChanged := !currentMissing && state.OriginalPresent && checksum(current) == state.OriginalSHA256 && uint32(currentMode.Perm()) != state.OriginalMode
		if !currentMatchesState(current, currentMissing, state) || (registry.ManageReplacements && originalModeChanged) {
			if !registry.ManageReplacements {
				return fmt.Errorf("refusing to overwrite concurrently changed %s", target)
			}

			adoptedState, err := adoptReplacementBaseline(options, statePath, state, current, currentMode)
			if err != nil {
				return fmt.Errorf("adopt replacement %s: %w", target, err)
			}

			state = adoptedState

			slog.Warn("adopted replacement containerd hosts file as new uninstall baseline",
				"registry", registry.Host,
				"path", target,
			)
		}
	}

	if !currentMissing && bytes.Equal(current, managed) && currentMode.Perm() == 0o644 && state.PendingManagedSHA256 == "" && state.ManagedSHA256 == managedSHA {
		if !state.Applied {
			state.Applied = true
			if err := writeJSONAtomic(statePath, state, 0o600); err != nil {
				return fmt.Errorf("complete route ownership update: %w", err)
			}
		}

		return nil
	}

	state.PendingManagedSHA256 = managedSHA
	if err := writeJSONAtomic(statePath, state, 0o600); err != nil {
		return fmt.Errorf("record pending route update: %w", err)
	}

	if err := writeFileAtomic(target, managed, 0o644); err != nil {
		return fmt.Errorf("write managed route: %w", err)
	}

	state.ManagedSHA256 = managedSHA
	state.PendingManagedSHA256 = ""

	state.Applied = true
	if err := writeJSONAtomic(statePath, state, 0o600); err != nil {
		return fmt.Errorf("complete route ownership update: %w", err)
	}

	return nil
}

func currentMatchesState(current []byte, missing bool, state routeState) bool {
	if missing {
		return true
	}

	currentSHA := checksum(current)

	return currentSHA == state.ManagedSHA256 ||
		(state.PendingManagedSHA256 != "" && currentSHA == state.PendingManagedSHA256) ||
		(state.OriginalPresent && currentSHA == state.OriginalSHA256)
}

func adoptReplacementBaseline(options Options, statePath string, state routeState, current []byte, mode fs.FileMode) (routeState, error) {
	state.OriginalPresent = true
	state.OriginalMode = uint32(mode.Perm())
	state.OriginalSHA256 = checksum(current)
	state.OriginalBackup = "original-hosts-" + state.OriginalSHA256 + ".toml"

	backupPath, err := backupFile(options.HostStateDir, state)
	if err != nil {
		return routeState{}, err
	}

	if err := writeFileAtomic(backupPath, current, 0o600); err != nil {
		return routeState{}, fmt.Errorf("write replacement backup: %w", err)
	}

	if err := writeJSONAtomic(statePath, state, 0o600); err != nil {
		return routeState{}, fmt.Errorf("record replacement baseline: %w", err)
	}

	return state, nil
}

func restoreRoute(options Options, state routeState) error {
	if err := ensureTargetDirectory(options, state.Host); err != nil {
		return err
	}

	target := targetPath(options, state.Host)
	current, _, err := readRegularFile(target)

	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return err
	}

	if !currentMatchesState(current, missing, state) {
		return fmt.Errorf("refusing to replace concurrently changed %s", target)
	}

	if state.OriginalPresent {
		backupPath, pathErr := backupFile(options.HostStateDir, state)
		if pathErr != nil {
			return pathErr
		}

		original, readErr := os.ReadFile(backupPath)
		if readErr != nil {
			return fmt.Errorf("read route backup: %w", readErr)
		}

		if checksum(original) != state.OriginalSHA256 {
			return errors.New("route backup checksum mismatch")
		}

		if missing || !bytes.Equal(current, original) {
			if err := writeFileAtomic(target, original, fs.FileMode(state.OriginalMode)); err != nil {
				return fmt.Errorf("restore route backup: %w", err)
			}
		}
	} else if !missing {
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove managed route: %w", err)
		}
	}

	if err := os.RemoveAll(stateDirectory(options.HostStateDir, state.Host)); err != nil {
		return fmt.Errorf("remove route state: %w", err)
	}

	if !state.OriginalPresent {
		_ = os.Remove(filepath.Dir(target)) //nolint:errcheck // directory cleanup is best effort when other containerd files remain
	}

	return nil
}

func loadStates(root string) ([]routeState, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read route state directory: %w", err)
	}

	states := make([]routeState, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		statePath := filepath.Join(root, entry.Name(), "state.json")

		state, err := readState(statePath)
		if errors.Is(err, fs.ErrNotExist) {
			if !validHex(entry.Name(), 16) {
				return nil, fmt.Errorf("unexpected directory in route state: %s", entry.Name())
			}

			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return nil, fmt.Errorf("remove uncommitted route state %s: %w", entry.Name(), err)
			}

			continue
		}

		if err != nil {
			return nil, err
		}

		if state.Version != stateVersion || !validChecksum(state.ManagedSHA256) ||
			(state.PendingManagedSHA256 != "" && !validChecksum(state.PendingManagedSHA256)) ||
			(state.OriginalPresent && !validChecksum(state.OriginalSHA256)) {
			return nil, fmt.Errorf("route state directory %s has invalid ownership metadata", entry.Name())
		}

		if state.OriginalPresent {
			backupPath, err := backupFile(root, state)
			if err != nil {
				return nil, fmt.Errorf("resolve route backup for state %s: %w", entry.Name(), err)
			}

			original, err := os.ReadFile(backupPath)
			if err != nil {
				return nil, fmt.Errorf("read route backup for state %s: %w", entry.Name(), err)
			}

			if checksum(original) != state.OriginalSHA256 {
				return nil, fmt.Errorf("route backup checksum mismatch for state %s", entry.Name())
			}
		}

		normalizedHost, err := NormalizeRegistryHost(state.Host)
		if err != nil || normalizedHost != state.Host {
			return nil, fmt.Errorf("route state directory %s has invalid host %q", entry.Name(), state.Host)
		}

		if routeKey(state.Host) != entry.Name() {
			return nil, fmt.Errorf("route state directory %s does not match host %s", entry.Name(), state.Host)
		}

		states = append(states, state)
	}

	return states, nil
}

func readState(path string) (routeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return routeState{}, err
	}

	var state routeState
	if err := json.Unmarshal(data, &state); err != nil {
		return routeState{}, fmt.Errorf("decode route state %s: %w", path, err)
	}

	return state, nil
}

func readRegularFile(path string) ([]byte, fs.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}

	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}

	return data, info.Mode(), nil
}

func ensureTargetDirectory(options Options, host string) error {
	directory := filepath.Join(options.HostCertsDir, host)

	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}

		info, err = os.Lstat(directory)
	}

	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", directory)
	}

	return nil
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	data = append(data, '\n')

	return writeFileAtomic(path, data, mode)
}

func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}

	temp, err := os.CreateTemp(directory, ".gantryctl-*")
	if err != nil {
		return err
	}

	tempPath := temp.Name()

	defer func() { _ = os.Remove(tempPath) }() //nolint:errcheck // temporary cleanup is best effort after rename or failure

	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close() //nolint:errcheck // preserve the original chmod error
		return err
	}

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close() //nolint:errcheck // preserve the original write error
		return err
	}

	if err := temp.Sync(); err != nil {
		_ = temp.Close() //nolint:errcheck // preserve the original sync error
		return err
	}

	if err := temp.Close(); err != nil {
		return err
	}

	return os.Rename(tempPath, path)
}

func targetPath(options Options, host string) string {
	return filepath.Join(options.HostCertsDir, host, "hosts.toml")
}

func stateDirectory(root, host string) string {
	return filepath.Join(root, routeKey(host))
}

func stateFile(root, host string) string {
	return filepath.Join(stateDirectory(root, host), "state.json")
}

func originalFile(root, host string) string {
	return filepath.Join(stateDirectory(root, host), "original-hosts.toml")
}

func backupFile(root string, state routeState) (string, error) {
	name := state.OriginalBackup
	if name == "" {
		return originalFile(root, state.Host), nil
	}

	if filepath.Base(name) != name || name == "." || name == ".." {
		return "", fmt.Errorf("invalid route backup name %q", name)
	}

	if name != "original-hosts-"+state.OriginalSHA256+".toml" {
		return "", fmt.Errorf("route backup name %q does not match its checksum", name)
	}

	return filepath.Join(stateDirectory(root, state.Host), name), nil
}

func routeKey(host string) string {
	digest := sha256.Sum256([]byte(host))
	return hex.EncodeToString(digest[:16])
}

func checksum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validChecksum(value string) bool {
	return validHex(value, sha256.Size)
}

func validHex(value string, byteCount int) bool {
	if len(value) != byteCount*2 {
		return false
	}

	_, err := hex.DecodeString(value)

	return err == nil
}
