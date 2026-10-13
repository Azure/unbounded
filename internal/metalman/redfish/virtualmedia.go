// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"

	"github.com/Azure/unbounded/internal/metalman/bootmedia"
)

var _ bootmedia.Client = (*Client)(nil)

var _ interface {
	bootmedia.Client
	DeleteVirtualMedia(context.Context, bootmedia.Source) error
} = (*Client)(nil)

var virtualMediaName = regexp.MustCompile(`^ub-[0-9a-f]{1,64}\.iso$`)

const (
	rackTftpPath    = rackManagerPath + "/Tftp/Actions/Tftp."
	remoteDrivePath = rackSystemPath + "/Actions/ComputerSystem.RemoteDrive"
)

// SupportsVirtualMedia reports support for the RSCM remote-drive workflow.
func (c *Client) SupportsVirtualMedia() bool {
	return c.rscm != nil
}

func validateVirtualMedia(source bootmedia.Source) error {
	if !virtualMediaName.MatchString(source.Name) {
		return fmt.Errorf("invalid reserved virtual-media filename %q", source.Name)
	}

	address, err := netip.ParseAddr(source.Server)
	if err != nil || !address.Is4() {
		return fmt.Errorf("virtual-media server must be a literal IPv4 address: %q", source.Server)
	}

	return nil
}

// StageVirtualMedia copies an ISO to rack storage without changing host power.
func (c *Client) StageVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	c.rscm.mu.Lock()
	defer c.rscm.mu.Unlock()

	_, err := rackRequest(ctx, c.rscm.root, http.MethodPost, rackTftpPath+"Get",
		map[string]string{"Server": source.Server, "File": source.Name})

	return err
}

// MountVirtualMedia mounts the staged ISO and may restore AC for an unavailable blade.
// This assumes ImageName resolves a rack TFTP filename. RSCM spec sections 9.8
// (page 35) and 10.31 (page 62) describe the actions, but do not state that link.
func (c *Client) MountVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	return c.rackMutate(ctx, http.MethodPost, remoteDrivePath+"Mount",
		map[string]string{"ImageName": source.Name}, true)
}

// SetVirtualMediaBoot selects UEFI before selecting the remote drive.
func (c *Client) SetVirtualMediaBoot(ctx context.Context) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	c.rscm.mu.Lock()
	defer c.rscm.mu.Unlock()

	if err := c.rackMutateLocked(ctx, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideMode": "UEFI"}}, true); err != nil {
		return err
	}

	return c.rackMutateLocked(ctx, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideTarget": "RemoteDrive"}}, true)
}

// CleanupVirtualMedia restores disk boot and unmounts without deleting the ISO.
// It never changes AC power. A confirmed AC-off blade needs no blade cleanup.
// Firmware responses for already-unmounted media need hardware validation;
// until then, an absence-like failure is not proof that cleanup succeeded.
func (c *Client) CleanupVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	c.rscm.mu.Lock()
	defer c.rscm.mu.Unlock()

	_, err := rackRequest(ctx, c.session, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideTarget": "Hdd"}})
	if err == nil {
		_, err = rackRequest(ctx, c.session, http.MethodPost, remoteDrivePath+"Unmount", nil)
	}

	if err != nil {
		if !rackUnavailable(err) {
			return err
		}

		state, powerErr := c.rscm.power(ctx)
		if powerErr != nil {
			return errors.Join(err, fmt.Errorf("checking AC power during media cleanup: %w", powerErr))
		}

		if state != PowerOff {
			return err
		}
	}

	return nil
}

// DeleteVirtualMedia deletes only the staged rack file, independently of the blade.
// Callers must persist boot-safe cleanup before deletion and retain ownership on error.
// The spec gives no standard already-absent result. Firmware responses need hardware
// validation before a failed retry after a lost response can be treated as success.
func (c *Client) DeleteVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	c.rscm.mu.Lock()
	defer c.rscm.mu.Unlock()

	_, err := rackRequest(ctx, c.rscm.root, http.MethodPost, rackTftpPath+"Delete",
		map[string]string{"File": source.Name})

	return err
}
