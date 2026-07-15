// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociinstance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/Azure/unbounded/internal/machineops"
)

func (p *Provider) executeReplace(ctx context.Context, client computeClient, instanceID string, request machineops.OperationRequest) (machineops.OperationResult, error) {
	if strings.TrimSpace(request.MachineName) == "" {
		return machineops.OperationResult{}, fmt.Errorf("machine name is required for OCI HostReplace")
	}

	// Reconcile retries may happen after providerID handoff but before cleanup.
	// Detect that state from replacement tags and return the pending cleanup work.
	current, getErr := client.GetInstance(ctx, instanceID)
	if getErr == nil {
		if isReplacementFor(current, request, current.FreeformTags[tagOldProviderID]) {
			return machineops.OperationResult{ProviderID: providerIDPrefix + instanceID, CleanupProviderID: current.FreeformTags[tagOldProviderID]}, nil
		}
	}

	return p.replaceHost(ctx, client, instanceID, request)
}

func (p *Provider) replaceHost(ctx context.Context, client computeClient, oldInstanceID string, request machineops.OperationRequest) (machineops.OperationResult, error) {
	if strings.TrimSpace(request.ReplaceUserData) == "" {
		return machineops.OperationResult{}, fmt.Errorf("replacement user data is required for OCI HostReplace")
	}

	oldInstance, err := replacementSourceInstance(ctx, client, oldInstanceID)
	if err != nil {
		return machineops.OperationResult{}, fmt.Errorf("inspect OCI replacement source %s: %w", oldInstanceID, err)
	}

	if err := rejectAttachedDataVolumes(ctx, client, oldInstance); err != nil {
		return machineops.OperationResult{}, fmt.Errorf("validate OCI replacement source %s: %w", oldInstanceID, err)
	}

	primaryVNIC, err := primaryVNIC(ctx, client, oldInstance)
	if err != nil {
		return machineops.OperationResult{}, fmt.Errorf("resolve primary OCI VNIC for %s: %w", oldInstanceID, err)
	}

	replacement, ok, err := p.findExistingReplacement(ctx, client, oldInstance, request)
	if err != nil {
		return machineops.OperationResult{}, fmt.Errorf("find existing OCI replacement for %s: %w", oldInstanceID, err)
	}

	if !ok {
		// Build all launch inputs before stopping the old host so validation
		// failures do not create an avoidable outage.
		launchDetails, err := p.buildReplacementLaunchDetails(ctx, client, oldInstance, primaryVNIC, request)
		if err != nil {
			return machineops.OperationResult{}, fmt.Errorf("build OCI replacement launch details for %s: %w", oldInstanceID, err)
		}

		// STOPPING still owns the old Node identity. Wait for STOPPED before
		// launching a replacement that will use the same kubelet node name.
		if !isStopped(oldInstance) && !isStopping(oldInstance) {
			if err := client.InstanceAction(ctx, oldInstanceID, instanceActionStop); err != nil {
				return machineops.OperationResult{}, fmt.Errorf("stop OCI source instance %s: %w", oldInstanceID, err)
			}
		}

		_, err = waitForInstanceState(ctx, client, oldInstance, core.InstanceLifecycleStateStopped)
		if err != nil {
			return machineops.OperationResult{}, fmt.Errorf("wait for OCI source instance %s to stop: %w", oldInstanceID, err)
		}

		replacement, err = client.LaunchInstance(ctx, launchDetails, retryToken(request, "launch-replacement"))
		if err != nil {
			return machineops.OperationResult{}, fmt.Errorf("launch OCI replacement instance: %w", err)
		}
	}

	running, err := waitForInstanceState(ctx, client, replacement, core.InstanceLifecycleStateRunning)
	if err != nil {
		return machineops.OperationResult{}, fmt.Errorf("wait for OCI replacement instance to run: %w", err)
	}

	if running.Id == nil || *running.Id == "" {
		return machineops.OperationResult{}, fmt.Errorf("OCI replacement instance has no ID")
	}

	return machineops.OperationResult{
		ProviderID:        providerIDPrefix + *running.Id,
		CleanupProviderID: providerIDPrefix + oldInstanceID,
	}, nil
}

func waitForInstanceState(ctx context.Context, client computeClient, instance core.Instance, target core.InstanceLifecycleStateEnum) (core.Instance, error) {
	if instance.Id == nil || *instance.Id == "" {
		return core.Instance{}, fmt.Errorf("OCI instance has no ID")
	}

	if instance.LifecycleState == target {
		return instance, nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, replacementPollTimeout)
	defer cancel()

	for {
		// Poll before sleeping so tests and fast OCI transitions do not wait a full
		// interval after the target state has already been reached.
		updated, err := client.GetInstance(waitCtx, *instance.Id)
		if err != nil {
			return core.Instance{}, fmt.Errorf("get OCI instance %s while waiting for %s: %w", *instance.Id, target, err)
		}

		if updated.LifecycleState == target {
			return updated, nil
		}

		if isTerminalInstance(updated) {
			return core.Instance{}, fmt.Errorf("OCI instance %s reached terminal state %s while waiting for %s", *instance.Id, updated.LifecycleState, target)
		}

		select {
		case <-waitCtx.Done():
			if waitCtx.Err() == context.DeadlineExceeded {
				return core.Instance{}, fmt.Errorf("timed out waiting for OCI instance %s to reach %s", *instance.Id, target)
			}

			return core.Instance{}, waitCtx.Err()
		case <-time.After(replacementPollInterval):
		}
	}
}

func isStopped(instance core.Instance) bool {
	return instance.LifecycleState == core.InstanceLifecycleStateStopped
}

func isStopping(instance core.Instance) bool {
	return instance.LifecycleState == core.InstanceLifecycleStateStopping
}

func isTerminalInstance(instance core.Instance) bool {
	return instance.LifecycleState == core.InstanceLifecycleStateTerminated || instance.LifecycleState == core.InstanceLifecycleStateTerminating
}

func isReplacementFor(instance core.Instance, request machineops.OperationRequest, oldProviderID string) bool {
	// Operation UID plus old providerID makes replacement lookup restart-safe and
	// prevents matching an unrelated instance from another Host Operation.
	return instance.FreeformTags[tagOperationUID] == string(request.OperationUID) && instance.FreeformTags[tagOldProviderID] == oldProviderID
}

func retryToken(request machineops.OperationRequest, action string) string {
	operationID := string(request.OperationUID)
	if operationID == "" {
		operationID = request.OperationName
	}

	return fmt.Sprintf("%s-%s-%s", retryTokenPrefix, action, operationID)
}
