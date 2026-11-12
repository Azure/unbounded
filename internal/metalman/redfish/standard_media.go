// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/metalman/bootmedia"
)

var errNoVirtualMediaDevices = fmt.Errorf("no CD or DVD virtual media device found: %w", ErrUnsupported)

// Standard BMCs pull the ISO by URL. VirtualMedia is linked from the system
// or from its managers (as on HPE iLO). Only CD/DVD devices are used.
type virtualMediaDevice struct {
	path     string
	image    string
	inserted bool
	insert   *redfishAction
	eject    *redfishAction
}

func (d virtualMediaDevice) holds(image string) bool {
	return d.inserted && d.image == image
}

func validateMediaURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return fmt.Errorf("virtual-media image URL must be an absolute HTTP or HTTPS URL without credentials: %q", raw)
	}

	return nil
}

func (c *Client) virtualMediaCollections(ctx context.Context) ([]string, error) {
	systemPath := c.systemBase()

	data, _, err := c.getResource(ctx, systemPath)
	if err != nil {
		return nil, err
	}

	var system struct {
		redfishLink
		VirtualMedia *redfishLink `json:"VirtualMedia"`
		Links        struct {
			ManagedBy []redfishLink `json:"ManagedBy"`
		} `json:"Links"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		return nil, fmt.Errorf("parsing system links: %w", err)
	}

	if system.ODataID != "" {
		systemPath, err = resourcePath(system.ODataID, systemPath)
		if err != nil {
			return nil, fmt.Errorf("parsing system link: %w", err)
		}
	}

	var collections []string

	add := func(id, base string) error {
		if id == "" {
			return nil
		}

		path, err := resourcePath(id, base)
		if err != nil {
			return fmt.Errorf("parsing VirtualMedia link: %w", err)
		}

		if !slices.Contains(collections, path) {
			collections = append(collections, path)
		}

		return nil
	}

	if system.VirtualMedia != nil {
		if err := add(system.VirtualMedia.ODataID, systemPath); err != nil {
			return nil, err
		}
	}

	for _, link := range system.Links.ManagedBy {
		managerPath, err := resourcePath(link.ODataID, systemPath)
		if err != nil {
			return nil, fmt.Errorf("parsing ManagedBy link: %w", err)
		}

		data, _, err := c.getResource(ctx, managerPath)
		if err != nil {
			return nil, err
		}

		var manager struct {
			VirtualMedia *redfishLink `json:"VirtualMedia"`
		}
		if err := json.Unmarshal(data, &manager); err != nil {
			return nil, fmt.Errorf("parsing manager %s: %w", managerPath, err)
		}

		if manager.VirtualMedia != nil {
			if err := add(manager.VirtualMedia.ODataID, managerPath); err != nil {
				return nil, err
			}
		}
	}

	return collections, nil
}

func (c *Client) virtualMediaDevices(ctx context.Context) ([]virtualMediaDevice, error) {
	collections, err := c.virtualMediaCollections(ctx)
	if err != nil {
		return nil, err
	}

	var devices []virtualMediaDevice

	for _, collectionPath := range collections {
		data, _, err := c.getResource(ctx, collectionPath)
		if err != nil {
			return nil, err
		}

		var collection struct {
			Members []redfishLink `json:"Members"`
		}
		if err := json.Unmarshal(data, &collection); err != nil {
			return nil, fmt.Errorf("parsing VirtualMedia collection %s: %w", collectionPath, err)
		}

		for _, member := range collection.Members {
			path, err := resourcePath(member.ODataID, collectionPath)
			if err != nil {
				return nil, fmt.Errorf("parsing VirtualMedia member link: %w", err)
			}

			if slices.ContainsFunc(devices, func(d virtualMediaDevice) bool {
				return resourceRequestPath(d.path) == resourceRequestPath(path)
			}) {
				continue
			}

			device, ok, err := c.virtualMediaDevice(ctx, path)
			if err != nil {
				return nil, err
			}

			if ok {
				devices = append(devices, device)
			}
		}
	}

	if len(devices) == 0 {
		return nil, errNoVirtualMediaDevices
	}

	return devices, nil
}

func (c *Client) virtualMediaDevice(ctx context.Context, path string) (virtualMediaDevice, bool, error) {
	data, _, err := c.getResource(ctx, path)
	if err != nil {
		return virtualMediaDevice{}, false, err
	}

	var media struct {
		MediaTypes []string `json:"MediaTypes"`
		Image      *string  `json:"Image"`
		Inserted   *bool    `json:"Inserted"`
		Actions    struct {
			Insert *redfishAction `json:"#VirtualMedia.InsertMedia"`
			Eject  *redfishAction `json:"#VirtualMedia.EjectMedia"`
		} `json:"Actions"`
	}
	if err := json.Unmarshal(data, &media); err != nil {
		return virtualMediaDevice{}, false, fmt.Errorf("parsing VirtualMedia %s: %w", path, err)
	}

	if !slices.Contains(media.MediaTypes, "CD") && !slices.Contains(media.MediaTypes, "DVD") {
		return virtualMediaDevice{}, false, nil
	}

	device := virtualMediaDevice{path: path, insert: media.Actions.Insert, eject: media.Actions.Eject}
	if media.Image != nil {
		device.image = *media.Image
	}

	if media.Inserted != nil {
		device.inserted = *media.Inserted
	} else {
		device.inserted = device.image != ""
	}

	if device.insert != nil && device.insert.Target == "" {
		device.insert = nil
	}

	if device.eject != nil && device.eject.Target == "" {
		device.eject = nil
	}

	return device, true, nil
}

// Staging checks capabilities without changing media or host power.
func (c *Client) standardStageVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	if err := validateMediaURL(source.URL); err != nil {
		return err
	}

	boot, err := c.systemBoot(ctx)
	if err != nil {
		return err
	}

	if _, err := virtualMediaBootTarget(boot.AllowedTargets); err != nil {
		return err
	}

	device, _, err := c.prepareVirtualMediaInsert(ctx, source)
	if err != nil {
		return err
	}

	_, err = device.ejectTarget()

	return err
}

func (c *Client) prepareVirtualMediaInsert(ctx context.Context, source bootmedia.Source) (virtualMediaDevice, map[string]any, error) {
	if err := validateMediaURL(source.URL); err != nil {
		return virtualMediaDevice{}, nil, err
	}

	devices, err := c.virtualMediaDevices(ctx)
	if err != nil {
		return virtualMediaDevice{}, nil, err
	}

	for _, device := range devices {
		if device.holds(source.URL) {
			return device, nil, nil
		}
	}

	for _, device := range devices {
		if device.image == source.URL {
			return device, nil, nil
		}
	}

	var device *virtualMediaDevice

	for i := range devices {
		if devices[i].insert == nil {
			continue
		}

		if device == nil || (device.inserted && !devices[i].inserted) {
			device = &devices[i]
		}
	}

	if device == nil {
		return virtualMediaDevice{}, nil, fmt.Errorf("no CD or DVD device advertises #VirtualMedia.InsertMedia: %w", ErrUnsupported)
	}

	if _, err := device.insertTarget(); err != nil {
		return virtualMediaDevice{}, nil, err
	}

	body, err := c.insertMediaBody(ctx, *device, source.URL)

	return *device, body, err
}

func (c *Client) standardMountVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	device, body, err := c.prepareVirtualMediaInsert(ctx, source)
	if err != nil {
		return err
	}

	if device.holds(source.URL) {
		return nil
	}

	if device.image == source.URL {
		return c.waitVirtualMediaInsert(ctx, device.path, source.URL, nil)
	}

	target, err := device.insertTarget()
	if err != nil {
		return err
	}

	if device.inserted {
		if err := c.ejectVirtualMedia(ctx, device); err != nil {
			return err
		}
	}

	resp, err := c.modify(ctx, http.MethodPost, target, body)
	if err != nil {
		return err
	}

	if !isSuccessStatus(resp.status) {
		return actionError(http.MethodPost, target, resp)
	}

	if resp.status == http.StatusAccepted {
		return c.waitVirtualMediaInsert(ctx, device.path, source.URL, resp.header)
	}

	inserted, ok, err := c.virtualMediaDevice(ctx, device.path)
	if err != nil {
		return err
	}

	if !ok || !inserted.holds(source.URL) {
		return fmt.Errorf("virtual media %s does not report image %s as inserted", device.path, source.URL)
	}

	return nil
}

func (c *Client) waitVirtualMediaInsert(ctx context.Context, path, image string, header http.Header) error {
	ctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()

	for {
		timer := time.NewTimer(retryAfter(header))

		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for virtual media %s to report image %s as inserted: %w", path, image, ctx.Err())
		case <-timer.C:
		}

		device, ok, err := c.virtualMediaDevice(ctx, path)
		if err != nil {
			return err
		}

		if ok && device.holds(image) {
			return nil
		}
	}
}

func (c *Client) insertMediaBody(ctx context.Context, device virtualMediaDevice, image string) (map[string]any, error) {
	params, err := c.actionParameters(ctx, *device.insert, device.path)
	if err != nil {
		return nil, err
	}

	if params == nil {
		return map[string]any{"Image": image}, nil
	}

	protocol := "HTTP"
	if strings.HasPrefix(image, "https:") {
		protocol = "HTTPS"
	}

	body := map[string]any{"Image": image}

	for _, param := range params {
		switch param.Name {
		case "Image":
		case "Inserted", "WriteProtected":
			if param.Required {
				body[param.Name] = true
			}
		case "TransferProtocolType":
			if len(param.AllowableValues) > 0 && !slices.Contains(param.AllowableValues, protocol) {
				return nil, fmt.Errorf("InsertMedia does not allow transfer protocol %s: %w", protocol, ErrUnsupported)
			}

			body["TransferProtocolType"] = protocol
		case "TransferMethod":
			if param.Required {
				if len(param.AllowableValues) > 0 && !slices.Contains(param.AllowableValues, "Stream") {
					return nil, fmt.Errorf("InsertMedia does not allow transfer method Stream: %w", ErrUnsupported)
				}

				body["TransferMethod"] = "Stream"
			}
		default:
			if param.Required {
				return nil, fmt.Errorf("InsertMedia requires unsupported parameter %s: %w", param.Name, ErrUnsupported)
			}
		}
	}

	return body, nil
}

func (d virtualMediaDevice) insertTarget() (string, error) {
	if d.insert == nil {
		return "", fmt.Errorf("virtual media %s does not advertise #VirtualMedia.InsertMedia: %w", d.path, ErrUnsupported)
	}

	target, err := actionPath(d.insert.Target, d.path)
	if err != nil {
		return "", fmt.Errorf("parsing InsertMedia target: %w", err)
	}

	return target, nil
}

func (d virtualMediaDevice) ejectTarget() (string, error) {
	if d.eject == nil {
		return "", fmt.Errorf("virtual media %s does not advertise #VirtualMedia.EjectMedia: %w", d.path, ErrUnsupported)
	}

	target, err := actionPath(d.eject.Target, d.path)
	if err != nil {
		return "", fmt.Errorf("parsing EjectMedia target: %w", err)
	}

	return target, nil
}

func (c *Client) ejectVirtualMedia(ctx context.Context, device virtualMediaDevice) error {
	target, err := device.ejectTarget()
	if err != nil {
		return err
	}

	resp, err := c.modify(ctx, http.MethodPost, target, map[string]any{})
	if err != nil {
		return err
	}

	if !isSuccessStatus(resp.status) {
		return actionError(http.MethodPost, target, resp)
	}

	ejected, ok, err := c.virtualMediaDevice(ctx, device.path)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("virtual media %s does not report a CD or DVD device after ejection", device.path)
	}

	if ejected.inserted {
		return fmt.Errorf("virtual media %s still reports media as inserted", device.path)
	}

	return nil
}

func virtualMediaBootTarget(allowed []BootTarget) (BootTarget, error) {
	if slices.Contains(allowed, BootTargetUsbCd) {
		return BootTargetUsbCd, nil
	}

	if len(allowed) == 0 || slices.Contains(allowed, BootTargetCd) {
		return BootTargetCd, nil
	}

	return "", fmt.Errorf("neither boot override target UsbCd nor Cd is allowed by the BMC (allowed %v): %w", allowed, ErrUnsupported)
}

func isVirtualMediaBootTarget(target BootTarget) bool {
	return target == BootTargetCd || target == BootTargetUsbCd
}

func (c *Client) standardSetVirtualMediaBoot(ctx context.Context) error {
	boot, err := c.systemBoot(ctx)
	if err != nil {
		return err
	}

	target, err := virtualMediaBootTarget(boot.AllowedTargets)
	if err != nil {
		return err
	}

	modeOK := !boot.HasMode || boot.Mode == BootModeUEFI
	if boot.Target == target && boot.Enabled == BootOnce && modeOK {
		return nil
	}

	settings := map[string]string{
		"BootSourceOverrideTarget": string(target), "BootSourceOverrideEnabled": string(BootOnce),
	}
	if !modeOK {
		settings["BootSourceOverrideMode"] = string(BootModeUEFI)
	}

	if err := c.patchBoot(ctx, settings); err != nil {
		return err
	}

	boot, err = c.systemBoot(ctx)
	if err != nil {
		return err
	}

	if boot.Target != target || boot.Enabled != BootOnce || (boot.HasMode && boot.Mode != BootModeUEFI) {
		return fmt.Errorf("boot override does not report one-time UEFI %s boot: %+v", target, boot)
	}

	return nil
}

// sameMediaCapability compares the complete capability path, not a filename
// suffix. Cleanup must still work after the advertised serve address changes.
func sameMediaCapability(image, source string) bool {
	if validateMediaURL(image) != nil || validateMediaURL(source) != nil {
		return false
	}

	a, err := url.Parse(image)
	if err != nil {
		return false
	}

	b, err := url.Parse(source)
	if err != nil {
		return false
	}

	parts := strings.Split(b.Path, "/")

	return len(parts) == 4 && parts[1] == "bootmedia" &&
		virtualMediaName.MatchString("ub-"+parts[2]+".iso") && len(parts[2]) == 32 &&
		parts[3] == "boot.iso" && a.Path == b.Path &&
		a.RawQuery == "" && a.Fragment == "" && a.RawPath == "" &&
		b.RawQuery == "" && b.Fragment == "" && b.RawPath == ""
}

func (c *Client) standardCleanupVirtualMedia(ctx context.Context, source bootmedia.Source) error {
	boot, err := c.systemBoot(ctx)
	if err != nil {
		return err
	}

	if isVirtualMediaBootTarget(boot.Target) && boot.Enabled != BootDisabled {
		settings := map[string]string{"BootSourceOverrideEnabled": string(BootDisabled)}
		if len(boot.AllowedTargets) == 0 || slices.Contains(boot.AllowedTargets, BootTargetNone) {
			settings["BootSourceOverrideTarget"] = string(BootTargetNone)
		}

		if err := c.patchBoot(ctx, settings); err != nil {
			return err
		}

		boot, err = c.systemBoot(ctx)
		if err != nil {
			return err
		}

		if isVirtualMediaBootTarget(boot.Target) && boot.Enabled != BootDisabled {
			return fmt.Errorf("virtual CD boot override is still enabled after cleanup")
		}
	}

	if source.URL == "" {
		return nil
	}

	devices, err := c.virtualMediaDevices(ctx)
	if errors.Is(err, errNoVirtualMediaDevices) {
		return nil
	}

	if err != nil {
		return err
	}

	for _, device := range devices {
		if device.inserted && device.image == "" {
			return fmt.Errorf("virtual media %s reports inserted media with an unknown image; cannot verify cleanup ownership", device.path)
		}

		if sameMediaCapability(device.image, source.URL) {
			// An owned image without Inserted may still be mounting after a
			// canceled request. Keep ownership until it settles or is cleared.
			if !device.inserted {
				return fmt.Errorf("virtual media %s reports the owned image without insertion; cannot verify cleanup while insertion may be pending", device.path)
			}

			if err := c.ejectVirtualMedia(ctx, device); err != nil {
				return err
			}
		}
	}

	return nil
}
