// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// Publication.
//
// R1 is short and absolute: install by rename(2), never write in place. racer
// watches the config's parent directory and reloads on any event that touches
// the file, so a partial write is a config racer will read - and a truncated
// protobuf either fails to parse, which costs a generation, or parses into
// something the length prefixes happen to allow, which is worse. rename(2) makes
// the file appear whole or not at all.
//
// The temporary file is created in the destination directory rather than in
// TMPDIR, because rename is only atomic within a filesystem and the config
// directory is routinely a tmpfs or an emptyDir that TMPDIR is not on.

// ConfigFileName is the file racer is pointed at inside the watched directory.
const ConfigFileName = "racer.binpb"

// Marshal encodes a NodeConfig for publication. It is deterministic so that an
// unchanged derivation renders byte for byte identically and never provokes a
// pointless reload.
func Marshal(cfg *racerconfig.NodeConfig) ([]byte, error) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}

	return data, nil
}

// Unmarshal decodes a published config, for reading back what we last installed.
func Unmarshal(data []byte) (*racerconfig.NodeConfig, error) {
	cfg := &racerconfig.NodeConfig{}
	if err := proto.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	return cfg, nil
}

// Publish validates a config and installs it atomically. It returns false when
// the config is byte-identical to what is already there, so callers can skip the
// generation bump and leave racer alone.
//
// prev is the config currently installed, or nil if there is none. Both
// validations run before anything touches the filesystem: R5 says reject our own
// candidate, and a candidate that never reaches the directory cannot be
// half-rejected by one node and accepted by another.
func Publish(path string, prev, next *racerconfig.NodeConfig) (bool, error) {
	data, err := Marshal(next)
	if err != nil {
		return false, err
	}

	// An identical candidate is a no-op, and it is checked first: what is already
	// installed was validated when it was installed, and re-checking it against
	// itself would fail the generation rule for no reason.
	if same, err := fileHasContent(path, data); err != nil {
		return false, err
	} else if same {
		return false, nil
	}

	if err := Validate(next); err != nil {
		return false, fmt.Errorf("candidate config is invalid: %w", err)
	}

	if err := ValidateTransition(prev, next); err != nil {
		return false, fmt.Errorf("candidate config is not a legal successor: %w", err)
	}

	if err := WriteFileAtomic(path, data); err != nil {
		return false, err
	}

	return true, nil
}

// ReadConfig loads the config currently installed at path. A missing file is not
// an error: it is a node that has never published, and the caller starts from
// generation one.
func ReadConfig(path string) (*racerconfig.NodeConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return Unmarshal(data)
}

// WriteFileAtomic writes data to path so that a reader watching the parent
// directory only ever sees the complete file appear as a single swap.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".racer-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}

	name := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()     //nolint:errcheck
		_ = os.Remove(name) //nolint:errcheck

		return fmt.Errorf("write %s: %w", name, err)
	}

	// Sync before rename. Without it a crash can leave the directory entry
	// pointing at a file whose contents never reached the disk, which is exactly
	// the partial config rename was supposed to rule out.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()     //nolint:errcheck
		_ = os.Remove(name) //nolint:errcheck

		return fmt.Errorf("sync %s: %w", name, err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(name) //nolint:errcheck

		return fmt.Errorf("close %s: %w", name, err)
	}

	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name) //nolint:errcheck

		return fmt.Errorf("chmod %s: %w", name, err)
	}

	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name) //nolint:errcheck

		return fmt.Errorf("rename %s to %s: %w", name, path, err)
	}

	return nil
}

func fileHasContent(path string, data []byte) (bool, error) {
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if len(existing) != len(data) {
		return false, nil
	}

	for i := range existing {
		if existing[i] != data[i] {
			return false, nil
		}
	}

	return true, nil
}
