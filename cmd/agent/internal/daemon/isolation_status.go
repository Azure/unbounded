// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
	"github.com/Azure/unbounded/internal/provision"
)

// isolationStatus is the agent-owned isolation part of the Machine status.
// An empty Applied or a nil Change leaves that field as it is.
type isolationStatus struct {
	// Applied is the mode the node runs in (status.isolation).
	Applied v1alpha3.IsolationMode
	// Change is the latest mode change attempt (status.isolationChange).
	Change *v1alpha3.IsolationChangeStatus
}

// patchIsolationStatus writes status.isolation and status.isolationChange in
// one JSON merge patch. The agent is the only writer of these fields, so the
// patch carries no resourceVersion: it cannot lose an optimistic lock race
// against machina or anyone else updating other status fields, and it does not
// read the Machine first, so a stale cache cannot make it skip a write. Both
// fields land together or not at all, so status.isolation never trails a
// Succeeded change.
//
// A Change replaces status.isolationChange as a whole: every optional field it
// leaves empty is sent as null, so a merge patch removes the value an earlier
// attempt wrote instead of keeping it. Without that, the reason of a refused
// change would outlive a later attempt that failed, and machina would report
// the failure as a refusal.
//
// The message is truncated to what the API accepts.
func patchIsolationStatus(ctx context.Context, c client.Client, machineName string, s isolationStatus) error {
	fields := map[string]any{}

	if s.Applied != "" {
		fields["isolation"] = s.Applied
	}

	if s.Change != nil {
		change, err := isolationChangePatch(*s.Change)
		if err != nil {
			return err
		}

		fields["isolationChange"] = change
	}

	if len(fields) == 0 {
		return nil
	}

	data, err := json.Marshal(map[string]any{"status": fields})
	if err != nil {
		return fmt.Errorf("encode isolation status patch: %w", err)
	}

	machine := &v1alpha3.Machine{}
	machine.Name = machineName

	if err := c.Status().Patch(ctx, machine, client.RawPatch(types.MergePatchType, data)); err != nil {
		return fmt.Errorf("patch isolation status of Machine %s: %w", machineName, err)
	}

	return nil
}

// isolationChangeOptionalFields are the JSON names of the optional fields of
// IsolationChangeStatus. Each one is sent as null when empty, so a write
// replaces the whole object. A test fails if a field is added to the type and
// not here.
var isolationChangeOptionalFields = []string{"strategy", "reason", "message", "lastTransitionTime"}

// isolationChangePatch encodes change as the merge patch value of
// status.isolationChange, with its message truncated and every empty optional
// field set to null.
func isolationChangePatch(change v1alpha3.IsolationChangeStatus) (map[string]any, error) {
	change.Message = machineops.TruncateUTF8Bytes(change.Message, v1alpha3.IsolationChangeMessageMaxLength)

	data, err := json.Marshal(change)
	if err != nil {
		return nil, fmt.Errorf("encode isolation change: %w", err)
	}

	value := map[string]any{}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode isolation change: %w", err)
	}

	for _, name := range isolationChangeOptionalFields {
		if _, ok := value[name]; !ok {
			value[name] = nil
		}
	}

	return value, nil
}

// reportMachineObservations reports everything the agent observes about its
// own installation to the Machine status. The applied isolation is best effort:
// it only feeds the drift condition, so a failure to report it (an older
// Machine CRD without the field, a missing status permission, an API error) is
// logged and never fails registration or daemon start.
func reportMachineObservations(ctx context.Context, log *slog.Logger, c client.Client, cfg *provision.AgentConfig) error {
	if err := reportProvisioningFormat(ctx, c, cfg); err != nil {
		return err
	}

	if err := reportAppliedIsolation(ctx, c, cfg); err != nil {
		log.Warn("could not report the applied isolation; the drift condition will not be set until it is reported",
			slog.String("machine", cfg.MachineName), slog.Any("error", err))
	}

	return nil
}

// reportAppliedIsolation reports the mode the node was applied with, so
// machina can show drift from spec.isolation. A config without the setting is
// nspawn. It never changes the node or the spec.
func reportAppliedIsolation(ctx context.Context, c client.Client, cfg *provision.AgentConfig) error {
	mode, err := cfg.IsolationMode()
	if err != nil {
		return err
	}

	return patchIsolationStatus(ctx, c, cfg.MachineName, isolationStatus{Applied: v1alpha3.IsolationMode(mode)})
}
