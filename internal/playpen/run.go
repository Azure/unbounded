// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Run configures the pod-local network and starts the VM. It blocks until QEMU
// exits or ctx is canceled.
func Run(ctx context.Context, cfg Config) (retErr error) {
	cfg, err := Normalize(cfg)
	if err != nil {
		return err
	}

	if err := validateKVM(cfg.KVMPath); err != nil {
		return err
	}

	if err := PrepareDisk(cfg); err != nil {
		return err
	}

	firmware, err := PrepareFirmware(cfg)
	if err != nil {
		return err
	}

	network, err := SetupNetwork(cfg)
	if err != nil {
		return err
	}

	defer func() {
		if err := network.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cleanup playpen network: %v\n", err)
		}
	}()

	vm := NewVMManager(cfg, network.TapFile, firmware)

	defer func() {
		if err := vm.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("stop VM: %w", err))
		}
	}()

	if err := vm.Start(ctx); err != nil {
		return err
	}

	return ServeBMC(ctx, cfg, vm)
}
