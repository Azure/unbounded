// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// On Ubuntu hosts, unattended-upgrades and needrestart will happily restart
// systemd-machined when a package they depend on (libcap2, systemd, ...) is
// upgraded, which kills the running systemd-nspawn@kube1 container and takes
// the node off the cluster. We mitigate that two ways:
//
//  1. An apt drop-in that blacklists the systemd / libcap packages from
//     unattended-upgrades. Security patches still flow for everything else;
//     these specific packages are upgraded only when an operator runs apt
//     interactively.
//  2. A needrestart drop-in that disables auto-restart of services after
//     package upgrades, in case any path bypasses the blacklist (manual
//     `apt upgrade`, third-party PPAs, etc.).
//
// Both files are idempotent and use distinctive 99- names so they win the
// last-write priority in /etc/apt/apt.conf.d and /etc/needrestart/conf.d.
//
//go:embed assets/99-unbounded-no-restart-systemd.conf
var aptUnattendedUpgradesConfig []byte

//go:embed assets/99-unbounded-needrestart.conf
var needrestartConfig []byte

const (
	aptDropInPath         = "/etc/apt/apt.conf.d/99-unbounded-no-restart-systemd"
	needrestartDropInPath = "/etc/needrestart/conf.d/99-unbounded.conf"
)

type hardenAPT struct {
	log *slog.Logger

	// Paths are overridable for tests; production code uses the constants
	// above via the HardenAPT() constructor.
	aptDropInPath         string
	needrestartDropInPath string
}

// HardenAPT returns a task that writes drop-ins which prevent
// unattended-upgrades and needrestart from restarting systemd-machined (and
// thereby killing the running nspawn container). Idempotent.
func HardenAPT(log *slog.Logger) phases.Task {
	return &hardenAPT{
		log:                   log,
		aptDropInPath:         aptDropInPath,
		needrestartDropInPath: needrestartDropInPath,
	}
}

func (h *hardenAPT) Name() string { return "harden-apt" }

func (h *hardenAPT) Do(_ context.Context) error {
	if err := utilio.WriteFile(h.aptDropInPath, aptUnattendedUpgradesConfig, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", h.aptDropInPath, err)
	}

	if err := utilio.WriteFile(h.needrestartDropInPath, needrestartConfig, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", h.needrestartDropInPath, err)
	}

	h.log.Info("apt and needrestart hardened against systemd-machined restarts",
		"apt_dropin", h.aptDropInPath,
		"needrestart_dropin", h.needrestartDropInPath,
	)

	return nil
}
