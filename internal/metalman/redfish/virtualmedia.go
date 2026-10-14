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
	"strings"

	"github.com/Azure/unbounded/internal/metalman/bootmedia"
)

var _ bootmedia.Client = (*Client)(nil)

var _ interface {
	bootmedia.Client
	DeleteVirtualMedia(context.Context, bootmedia.Source) error
} = (*Client)(nil)

// Tested rack manager firmware requires image names shorter than 40 characters.
var virtualMediaName = regexp.MustCompile(`^ub-[0-9a-f]{1,32}\.iso$`)

const (
	rackTftpPath    = rackManagerPath + "/Tftp/Actions/Tftp."
	remoteDrivePath = rackSystemPath + "/Actions/ComputerSystem.RemoteDrive"
)

// SupportsVirtualMedia reports support for the rack manager remote-drive workflow.
func (c *Client) SupportsVirtualMedia() bool {
	return c.rack != nil
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

// StageVirtualMedia copies the ISO to rack storage under its name and its
// primer name, without changing host power.
func (c *Client) StageVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	c.rack.mu.Lock()
	defer c.rack.mu.Unlock()

	for _, name := range []string{bootmedia.PrimerName(source.Name), source.Name} {
		if _, err := rackRequest(ctx, c.rack.root, http.MethodPost, rackTftpPath+"Get",
			map[string]string{"Server": source.Server, "File": name}); err != nil {
			return err
		}
	}

	return nil
}

// MountVirtualMedia mounts the staged ISO without changing host power.
// It mounts the primer name first. On tested firmware, after a power cycle
// that follows a media boot, the first mount of a new name often leaves no
// drive or an unreadable one; the second mount of another name attaches a
// working drive. Each mount takes about 20 seconds.
// Slot power-on also boots the host, so it must not run before media setup completes.
// ImageName names a file staged in the rack manager TFTP store.
func (c *Client) MountVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	for _, name := range []string{bootmedia.PrimerName(source.Name), source.Name} {
		if err := c.rackMutate(ctx, http.MethodPost, remoteDrivePath+"Mount",
			map[string]string{"ImageName": name}, false); err != nil {
			return err
		}
	}

	return nil
}

// SetVirtualMediaBoot selects UEFI before selecting the remote drive.
func (c *Client) SetVirtualMediaBoot(ctx context.Context) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	c.rack.mu.Lock()
	defer c.rack.mu.Unlock()

	if err := c.rackMutateLocked(ctx, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideMode": "UEFI"}}, false); err != nil {
		return err
	}

	return c.rackMutateLocked(ctx, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideTarget": "RemoteDrive"}}, false)
}

// CleanupVirtualMedia restores disk boot and unmounts without deleting the ISO.
// It never changes host power and requires both commands to succeed. Rack Off
// reports host power, not BMC or mount state, and cannot prove cleanup succeeded.
func (c *Client) CleanupVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	c.rack.mu.Lock()
	defer c.rack.mu.Unlock()

	if _, err := rackRequest(ctx, c.session, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideTarget": "Hdd"}}); err != nil {
		return err
	}

	_, err := rackRequest(ctx, c.session, http.MethodPost, remoteDrivePath+"Unmount", nil)

	// When nothing is mounted, for example after a power cycle, tested firmware
	// fails the unmount with a "... support is not enabled" message. There is
	// nothing to unmount.
	var failure *completionFailure
	if errors.As(err, &failure) && strings.Contains(strings.ToLower(failure.description), "support is not enabled") {
		return nil
	}

	return err
}

// DeleteVirtualMedia deletes only the staged rack files, independently of the blade.
// Callers must persist boot-safe cleanup before deletion and retain ownership on error.
// Tested firmware reports an already-deleted file as Failure with "No such file or directory".
func (c *Client) DeleteVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if !c.SupportsVirtualMedia() {
		return ErrUnsupported
	}

	if err := validateVirtualMedia(source); err != nil {
		return err
	}

	c.rack.mu.Lock()
	defer c.rack.mu.Unlock()

	for _, name := range []string{bootmedia.PrimerName(source.Name), source.Name} {
		_, err := rackRequest(ctx, c.rack.root, http.MethodPost, rackTftpPath+"Delete",
			map[string]string{"File": name})

		var failure *completionFailure
		if errors.As(err, &failure) && strings.Contains(strings.ToLower(failure.description), "no such file or directory") {
			continue
		}

		if err != nil {
			return err
		}
	}

	return nil
}
