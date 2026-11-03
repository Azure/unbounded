// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"sigs.k8s.io/yaml"
)

const (
	installDir         = "/opt/unbounded/registry-auth"
	providerPath       = installDir + "/bin/" + ProviderName
	providerConfigPath = installDir + "/credential-provider.json"
	azureConfigPath    = installDir + "/azure.json"
	proxyCAPath        = "/etc/kubernetes/certs/ca.crt"
	ownedCAPath        = installDir + "/proxy-ca.crt"
	ownerPath          = installDir + "/.owner"
	restartPath        = installDir + "/.restart-required"
	dropInPath         = "/etc/systemd/system/kubelet.service.d/90-unbounded-registry-auth.conf"
	unitPath           = "/etc/systemd/system/kubelet.service"
	managedMarker      = "# Managed by unbounded-operator registry authentication.\n"
)

// Installer owns only its dedicated directory and kubelet drop-in. It never
// edits the agent-generated unit, its drop-ins, or containerd's mirror route.
// Each node reconciles independently; only changed configuration restarts kubelet.
type Installer struct {
	Root        string
	Executable  string
	HostCommand func(context.Context, ...string) error
	KubeletArgs func() ([]string, error)
}

// Reconcile converges the owned files and verifies that kubelet is using them.
func (i *Installer) Reconcile(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	if cfg.Mode == ExternalMode {
		return i.remove(ctx)
	}

	args, err := i.KubeletArgs()
	if err != nil {
		owner, ownerErr := os.ReadFile(i.path(ownerPath))
		if !errors.Is(err, ErrKubeletNotRunning) || ownerErr != nil || string(owner) != managedMarker {
			return err
		}
	}

	for _, arg := range args {
		if arg == "--image-credential-provider-config" || arg == "--image-credential-provider-bin-dir" {
			return errors.New("kubelet already uses an external credential provider; preserve it with node-auth mode external")
		}

		if err := i.checkPodTokenFeature(args); err != nil {
			return err
		}

		if strings.HasPrefix(arg, "--image-credential-provider-config=") && arg != "--image-credential-provider-config="+providerConfigPath {
			return errors.New("kubelet already uses an external credential provider; preserve it with node-auth mode external")
		}

		if strings.HasPrefix(arg, "--image-credential-provider-bin-dir=") && arg != "--image-credential-provider-bin-dir="+installDir+"/bin" {
			return errors.New("kubelet already uses an external credential provider binary directory; preserve it with node-auth mode external")
		}
	}

	if existing, err := os.ReadFile(i.path(dropInPath)); err == nil {
		if !bytes.HasPrefix(existing, []byte(managedMarker)) {
			return errors.New("refusing to overwrite an unowned registry-auth kubelet drop-in")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	dropIn, err := i.dropIn()
	if err != nil {
		return err
	}

	if err := i.claim(); err != nil {
		return err
	}

	if cfg.CACert != "" {
		if err := i.checkPath(proxyCAPath); err != nil {
			return err
		}

		existing, err := os.ReadFile(i.path(proxyCAPath))

		owned, ownedErr := os.ReadFile(i.path(ownedCAPath))
		if ownedErr != nil && !errors.Is(ownedErr, os.ErrNotExist) {
			return ownedErr
		}

		if err == nil && !bytes.Equal(existing, []byte(cfg.CACert)) && (ownedErr != nil || !bytes.Equal(existing, owned)) {
			return errors.New("stock ACR provider CA path already contains a different certificate; refusing to overwrite it")
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		created := errors.Is(err, os.ErrNotExist)
		if created {
			if _, err := i.write(ownedCAPath, []byte(cfg.CACert), 0o644); err != nil {
				return err
			}
		}

		if _, err := i.write(proxyCAPath, []byte(cfg.CACert), 0o644); err != nil {
			return err
		}

		if !created && ownedErr == nil {
			if _, err := i.write(ownedCAPath, []byte(cfg.CACert), 0o644); err != nil {
				return err
			}
		}
	}

	azureConfig, err := cfg.AzureConfig()
	if err != nil {
		return err
	}

	if _, err := i.write(azureConfigPath, azureConfig, 0o644); err != nil {
		return err
	}

	providerConfig, err := cfg.ProviderConfig()
	if err != nil {
		return err
	}

	changed, err := i.write(providerConfigPath, providerConfig, 0o644)
	if err != nil {
		return err
	}

	binaryChanged, err := i.copyExecutable()
	if err != nil {
		return err
	}

	unitChanged, err := i.write(dropInPath, dropIn, 0o644)
	if err != nil {
		return err
	}

	_, pendingErr := os.Stat(i.path(restartPath))
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return pendingErr
	}

	if changed || binaryChanged || unitChanged || pendingErr == nil || !hasProviderArgs(args) {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := i.HostCommand(ctx, "daemon-reload"); err != nil {
			return err
		}

		if err := i.HostCommand(ctx, "restart", "kubelet.service"); err != nil {
			return err
		}
	}

	args, err = i.KubeletArgs()
	if err != nil {
		return err
	}

	if !hasProviderArgs(args) {
		return errors.New("kubelet has not activated the managed ACR credential-provider flags")
	}

	if err := i.checkPodTokenFeature(args); err != nil {
		return err
	}

	if err := os.Remove(i.path(restartPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (i *Installer) checkPodTokenFeature(args []string) error {
	const feature = "KubeletServiceAccountTokenForCredentialProviders"

	var configPath, gateFlags string

	for index, arg := range args {
		for _, flag := range []string{"--config", "--feature-gates"} {
			value, matched := strings.CutPrefix(arg, flag+"=")
			if arg == flag {
				if index+1 == len(args) {
					return fmt.Errorf("kubelet flag %s has no value", flag)
				}

				value, matched = args[index+1], true
			}

			if matched {
				if flag == "--config" {
					configPath = value
				} else {
					gateFlags = value
				}
			}
		}
	}

	enabled := true

	if configPath != "" {
		if !filepath.IsAbs(configPath) {
			return errors.New("cannot verify kubelet feature gates from a relative configuration path")
		}

		if err := i.checkPath(configPath); err != nil {
			return err
		}

		data, err := os.ReadFile(i.path(configPath))
		if err != nil {
			return fmt.Errorf("read kubelet feature gates: %w", err)
		}

		var config struct {
			FeatureGates map[string]bool `json:"featureGates"`
		}
		if err := yaml.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("parse kubelet feature gates: %w", err)
		}

		if value, ok := config.FeatureGates[feature]; ok {
			enabled = value
		}
	}

	for _, entry := range strings.Split(gateFlags, ",") {
		key, value, _ := strings.Cut(entry, "=")
		if key == feature {
			var err error

			enabled, err = strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("parse kubelet Pod-token feature flag: %w", err)
			}
		}
	}

	if !enabled {
		return errors.New("kubelet ServiceAccount tokens for credential providers are explicitly disabled")
	}

	return nil
}

func hasProviderArgs(args []string) bool {
	config, binary := false, false
	for _, arg := range args {
		config = config || arg == "--image-credential-provider-config="+providerConfigPath
		binary = binary || arg == "--image-credential-provider-bin-dir="+installDir+"/bin"
	}

	return config && binary
}

func usesManagedProvider(args []string) bool {
	for _, arg := range args {
		if arg == "--image-credential-provider-config="+providerConfigPath || arg == "--image-credential-provider-bin-dir="+installDir+"/bin" {
			return true
		}
	}

	return false
}

func (i *Installer) path(path string) string { return filepath.Join(i.Root, path) }

func (i *Installer) claim() error {
	owner, err := os.ReadFile(i.path(ownerPath))
	if err == nil {
		if string(owner) != managedMarker {
			return errors.New("registry-auth installation directory has an unknown owner")
		}

		return nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if entries, err := os.ReadDir(i.path(installDir)); err == nil && len(entries) != 0 {
		return errors.New("refusing to take over an unowned registry-auth installation directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if _, err := os.Lstat(i.path(dropInPath)); err == nil {
		return errors.New("refusing to take over an unowned registry-auth kubelet drop-in")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	_, err = i.write(ownerPath, []byte(managedMarker), 0o644)

	return err
}

// dropIn preserves the agent's complete ExecStart expression, including all
// environment expansions. Its original unit remains authoritative on every pass.
func (i *Installer) dropIn() ([]byte, error) {
	unit, err := os.ReadFile(i.path(unitPath))
	if err != nil {
		return nil, fmt.Errorf("read agent kubelet unit: %w", err)
	}

	var command string

	for _, line := range strings.Split(strings.ReplaceAll(string(unit), "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}

		if command != "" {
			return nil, errors.New("kubelet unit has multiple ExecStart directives")
		}

		command = strings.TrimPrefix(line, "ExecStart=")
	}

	if command == "" || !strings.Contains(command, "$KUBELET_NODE_CONFIG_ARGS") || strings.Contains(command, "--image-credential-provider-") {
		return nil, errors.New("unsupported kubelet unit; expected the agent-managed ExecStart without an existing credential provider")
	}

	entries, err := os.ReadDir(i.path(filepath.Dir(dropInPath)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	for _, entry := range entries {
		if entry.Name() == filepath.Base(dropInPath) || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}

		data, err := os.ReadFile(i.path(filepath.Join(filepath.Dir(dropInPath), entry.Name())))
		if err != nil {
			return nil, err
		}

		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "ExecStart=") || (!strings.HasPrefix(line, "#") && strings.Contains(line, "--image-credential-provider-")) {
				return nil, fmt.Errorf("kubelet drop-in %s owns ExecStart or credential-provider flags; use node-auth mode external", entry.Name())
			}
		}
	}

	return []byte(managedMarker + "[Service]\nExecStart=\nExecStart=" + command + " --image-credential-provider-config=" + providerConfigPath + " --image-credential-provider-bin-dir=" + installDir + "/bin\n"), nil
}

func (i *Installer) remove(ctx context.Context) error {
	if err := i.checkPath(ownerPath); err != nil {
		return err
	}

	owner, err := os.ReadFile(i.path(ownerPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if string(owner) != managedMarker {
		return errors.New("refusing to remove registry-auth files with an unknown owner")
	}

	for _, path := range []string{dropInPath, providerPath, providerConfigPath, azureConfigPath, ownedCAPath, proxyCAPath, restartPath} {
		if err := i.checkPath(path); err != nil {
			return err
		}
	}

	data, err := os.ReadFile(i.path(dropInPath))
	if err == nil && !bytes.HasPrefix(data, []byte(managedMarker)) {
		return errors.New("refusing to remove an unowned kubelet drop-in")
	}

	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err == nil {
		if _, err := i.write(restartPath, []byte(managedMarker), 0o600); err != nil {
			return err
		}

		if err := os.Remove(i.path(dropInPath)); err != nil {
			return err
		}
	}

	args, err := i.KubeletArgs()
	if err != nil && !errors.Is(err, ErrKubeletNotRunning) {
		return err
	}

	_, pendingErr := os.Stat(i.path(restartPath))
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return pendingErr
	}

	if data != nil || usesManagedProvider(args) || pendingErr == nil {
		if err := i.HostCommand(ctx, "daemon-reload"); err != nil {
			return err
		}

		if err := i.HostCommand(ctx, "restart", "kubelet.service"); err != nil {
			return err
		}

		args, err = i.KubeletArgs()
		if err != nil {
			return err
		}

		if usesManagedProvider(args) {
			return errors.New("kubelet still uses the managed credential provider; leaving provider files in place")
		}
	}
	// Never recursively remove a directory that another actor might have added to.
	if owned, err := os.ReadFile(i.path(ownedCAPath)); err == nil {
		existing, err := os.ReadFile(i.path(proxyCAPath))
		if err == nil {
			if !bytes.Equal(existing, owned) {
				return errors.New("managed proxy CA changed ownership; refusing to remove it")
			}

			if err := os.Remove(i.path(proxyCAPath)); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	for _, path := range []string{providerPath, providerConfigPath, azureConfigPath, ownedCAPath, restartPath, ownerPath} {
		if err := os.Remove(i.path(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Empty directories are removed non-recursively so a later opted-in
	// installation can reclaim the same paths without touching foreign files.
	for _, path := range []string{installDir + "/bin", installDir} {
		if err := os.Remove(i.path(path)); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}

	return nil
}

// safeParent rejects symlinks in operator-owned paths before atomic replacement.
func (i *Installer) safeParent(path string) error {
	if err := i.checkPath(path); err != nil {
		return err
	}

	return os.MkdirAll(filepath.Dir(i.path(path)), 0o755)
}

func (i *Installer) checkPath(path string) error {
	parent := i.Root
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		parent = filepath.Join(parent, part)

		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in registry-auth path %s", path)
		}
	}

	return nil
}

func (i *Installer) write(path string, data []byte, mode fs.FileMode) (bool, error) {
	if err := i.safeParent(path); err != nil {
		return false, err
	}

	existing, err := os.ReadFile(i.path(path))
	if err == nil && bytes.Equal(existing, data) {
		info, err := os.Stat(i.path(path))
		if err != nil {
			return false, err
		}

		if info.Mode().Perm() == mode {
			return false, nil
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	if err := i.replace(path, mode, func(dst io.Writer) error { _, err := dst.Write(data); return err }); err != nil {
		return false, err
	}

	return true, nil
}

func (i *Installer) copyExecutable() (bool, error) {
	if err := i.safeParent(providerPath); err != nil {
		return false, err
	}

	source, err := os.Open(i.Executable)
	if err != nil {
		return false, err
	}
	defer closeQuietly(source)

	want := sha256.New()
	if _, err := io.Copy(want, source); err != nil {
		return false, err
	}

	if existing, err := os.Open(i.path(providerPath)); err == nil {
		got := sha256.New()
		_, copyErr := io.Copy(got, existing)
		info, statErr := existing.Stat()
		closeErr := existing.Close()

		if copyErr == nil && statErr == nil && closeErr == nil && bytes.Equal(want.Sum(nil), got.Sum(nil)) && info.Mode().Perm() == 0o755 {
			return false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	if err := i.replace(providerPath, 0o755, func(dst io.Writer) error { _, err := io.Copy(dst, source); return err }); err != nil {
		return false, err
	}

	return true, nil
}

func (i *Installer) replace(path string, mode fs.FileMode, write func(io.Writer) error) error {
	// Persist restart intent before changing any active provider file. If a
	// restart or lease renewal fails, the next pass must retry even when the
	// files already match and the old provider flags are still present.
	if path != ownerPath && path != restartPath {
		if _, err := i.write(restartPath, []byte(managedMarker), 0o600); err != nil {
			return err
		}
	}

	file, err := os.CreateTemp(filepath.Dir(i.path(path)), ".registry-auth-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(file.Name()) //nolint:errcheck // Best-effort cleanup; a successful rename already removed the temporary path.
	}()
	defer closeQuietly(file)

	if err := write(file); err != nil {
		return err
	}

	if err := file.Chmod(mode); err != nil {
		return err
	}

	if err := file.Sync(); err != nil {
		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(file.Name(), i.path(path))
}
