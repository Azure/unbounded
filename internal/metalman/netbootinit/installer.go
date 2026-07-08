// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultSysfsRoot     = "/sys"
	defaultProcCmdline   = "/proc/cmdline"
	defaultMountRoot     = "/mnt"
	defaultESPMountPoint = "/mnt/esp"
)

// Installer implements the metalman netboot init process. It is written so the
// hardware-facing pieces are small wrappers around testable decision logic.
type Installer struct {
	SysfsRoot     string
	ProcCmdline   string
	MountRoot     string
	ESPMountPoint string
	Logger        *Logger
	Runner        CommandRunner
	HTTPClient    *http.Client
	Sleep         func(time.Duration)
}

// NewInstaller returns an installer configured for the real initrd environment.
func NewInstaller() *Installer {
	return &Installer{
		SysfsRoot:     defaultSysfsRoot,
		ProcCmdline:   defaultProcCmdline,
		MountRoot:     defaultMountRoot,
		ESPMountPoint: defaultESPMountPoint,
		Runner:        realCommandRunner{},
		HTTPClient:    http.DefaultClient,
		Sleep:         time.Sleep,
	}
}

func (i *Installer) normalize() {
	if i.SysfsRoot == "" {
		i.SysfsRoot = defaultSysfsRoot
	}

	if i.ProcCmdline == "" {
		i.ProcCmdline = defaultProcCmdline
	}

	if i.MountRoot == "" {
		i.MountRoot = defaultMountRoot
	}

	if i.ESPMountPoint == "" {
		i.ESPMountPoint = defaultESPMountPoint
	}

	if i.Runner == nil {
		i.Runner = realCommandRunner{}
	}

	if i.HTTPClient == nil {
		i.HTTPClient = http.DefaultClient
	}

	if i.Sleep == nil {
		i.Sleep = time.Sleep
	}
}

// Run performs the full netboot install flow.
func (i *Installer) Run(ctx context.Context) error {
	i.normalize()

	if err := i.setupMounts(); err != nil {
		return err
	}

	if i.Logger == nil {
		i.Logger = NewKernelLogger()
	}

	if err := os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin"); err != nil {
		return fmt.Errorf("setting PATH: %w", err)
	}

	i.Logger.Printf("installer starting")

	if err := i.loadKernelModules(ctx); err != nil {
		return err
	}

	cfg, err := i.readInstallConfig()
	if err != nil {
		return err
	}

	i.logInterfaces()

	iface, err := i.selectInterface(ctx, cfg.BootMAC)
	if err != nil {
		return err
	}

	if cfg.IPParam != "" {
		if err := i.configureStaticIP(ctx, iface, cfg.IPParam); err != nil {
			return err
		}
	}

	targetDisk, err := i.selectTargetDisk(ctx, cfg.TargetDisk)
	if err != nil {
		return err
	}

	i.Logger.Printf("target disk: %s", targetDisk)
	i.Logger.Printf("downloading disk image from %s", cfg.ImageURL)

	if err := retry(ctx, 120, 5*time.Second, "download and write disk image", i.Sleep, i.Logger, func() error {
		return i.downloadAndWriteImage(ctx, cfg.ImageURL, targetDisk)
	}); err != nil {
		return fmt.Errorf("failed to download and write disk image: %w", err)
	}

	unix.Sync()

	if err := retry(ctx, 5, 2*time.Second, "re-read partition table", i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "blockdev", "--rereadpt", targetDisk)
	}); err != nil {
		i.Logger.Printf("WARNING: could not re-read partition table")
	}

	i.Sleep(2 * time.Second)

	if cfg.CloudInit.DSURL != "" {
		if err := i.injectCloudInit(ctx, targetDisk, cfg.CloudInit); err != nil {
			return err
		}
	}

	if err := i.createUEFIBootEntry(ctx, targetDisk); err != nil {
		i.Logger.Printf("WARNING: %v", err)
	}

	if cfg.ServeURL != "" {
		i.Logger.Printf("disabling PXE boot")

		if err := retry(ctx, 5, 2*time.Second, "disable PXE", i.Sleep, i.Logger, func() error {
			return i.disablePXE(ctx, cfg.ServeURL)
		}); err != nil {
			i.Logger.Printf("WARNING: failed to disable PXE boot")
		}
	}

	i.Logger.Printf("installation complete, rebooting")
	i.Sleep(2 * time.Second)

	if err := retry(ctx, 3, 2*time.Second, "reboot", i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "reboot", "-f")
	}); err != nil {
		return fmt.Errorf("failed to reboot: %w", err)
	}

	return nil
}

// Fatal logs a fatal error and drops to a shell when one is available, matching
// the old init script's debugging behavior.
func (i *Installer) Fatal(err error) {
	i.normalize()

	if i.Logger == nil {
		i.Logger = NewKernelLogger()
	}

	i.Logger.Printf("FATAL: %v", err)

	shell, lookErr := i.Runner.LookPath("sh")
	if lookErr != nil {
		shell = "/bin/sh"
	}

	if _, statErr := os.Stat(shell); statErr == nil {
		if execErr := syscall.Exec(shell, []string{"sh"}, os.Environ()); execErr != nil {
			i.Logger.Printf("WARNING: failed to exec shell: %v", execErr)
		}
	}

	os.Exit(1)
}
