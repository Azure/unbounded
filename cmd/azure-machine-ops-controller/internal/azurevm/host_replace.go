// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

// Azure tag keys HostReplace stamps on the VM. Their values are the
// MachineOperation UID, which is not secret.
const (
	// TagReplacePending marks a VM that the operation prepared for deletion.
	// It proves that a VM found in the Deleting state is being deleted by
	// this replacement and not by someone else.
	TagReplacePending = "unbounded-replace-pending"
	// TagReplaceRestore records, next to TagReplacePending, the delete
	// options the VM had before the preparation changed them. A replacement
	// that fails between the preparation and the delete leaves the VM
	// prepared, and the next replacement restores the options from this tag.
	TagReplaceRestore = "unbounded-replace-restore"
	// TagReplacedBy marks the replacement VM with the operation that created
	// it, so Poll tells it apart from the VM it replaces.
	TagReplacedBy = "unbounded-replaced-by"
)

const (
	// Azure rejects longer tag values.
	azureTagValueMaxLength = 256
	// Azure rejects longer managed disk names.
	azureDiskNameMaxLength = 80
	// replaceModelMaxBytes bounds the VM model in either form: encoded in the
	// resume token, which is stored in the MachineOperation status, and
	// decoded from it.
	replaceModelMaxBytes = 256 << 10
)

// Failure reasons specific to HostReplace.
const (
	// reasonUnsupportedVM refuses a VM the replacement cannot faithfully
	// recreate. It is reported before anything is changed.
	reasonUnsupportedVM = "UnsupportedVM"
	// reasonReplacementLost reports a VM that was deleted for replacement
	// and could not be recreated. It must be recreated manually.
	reasonReplacementLost = "ReplacementLost"
)

// replaceHandle is the resume token of a HostReplace. It is everything Poll
// needs once the original VM is gone, and it holds no secret: the cloud-init
// is not part of the model, and Azure never returns a VM's administrator
// password or customData.
type replaceHandle struct {
	// OriginalVMID is Azure's unique ID of the VM that Begin observed. A VM
	// with the same name and another ID is not the one to replace.
	OriginalVMID string `json:"originalVMID"`
	// OriginalOSDiskID is the OS disk that VM had. The replacement gets a
	// new OS disk and this one is deleted with the VM, so a VM whose OS disk
	// was swapped since Begin must not be deleted.
	OriginalOSDiskID string `json:"originalOSDiskID"`
	// Model is the replacement VM as it will be created, without customData:
	// its JSON, gzip-compressed and base64-encoded.
	Model string `json:"model"`
}

// originalOSDiskID returns the resource ID of the VM's managed OS disk.
func originalOSDiskID(vm *armcompute.VirtualMachine) string {
	if vm.Properties == nil {
		return ""
	}

	return osDiskID(vm.Properties.StorageProfile)
}

// BeginReplace starts the replacement of an Azure VM: the VM is deleted and
// created again under the same name from its own model, with a fresh OS disk
// built from the host image and the replacement cloud-init as customData.
// The NICs and managed data disks are detached before the delete and attached
// to the replacement.
//
// Begin changes nothing in Azure. It checks the VM for every shape the
// replacement cannot faithfully recreate (see validateReplaceable), so such a
// VM fails the operation while it is still intact, and returns the
// replacement model in the handle. Sending nothing makes Begin safe to run
// again, and it means the engine has persisted the model before the VM is
// touched. PollReplace does the work.
func (p *Provider) BeginReplace(ctx context.Context, request machineops.OperationRequest) (machineops.BeginResult, error) {
	target, err := hostTarget(request, nil)
	if err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	if strings.TrimSpace(string(request.OperationUID)) == "" {
		return machineops.BeginResult{}, invalidRequest(errors.New("operation UID is required for Azure VM HostReplace"))
	}

	if err := validateReplaceUserData(request.ReplaceUserData); err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	if _, err := parseAzureImageReference(request.HostImage); err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	client, err := p.client(target.SubscriptionID, request.Auth)
	if err != nil {
		return machineops.BeginResult{}, fmt.Errorf("create Azure VM client: %w", err)
	}

	vm, err := client.GetVM(ctx, target.ResourceGroup, target.VMName, false)
	if err != nil {
		return machineops.BeginResult{}, beforeCreate(err)
	}

	if vm == nil {
		return machineops.BeginResult{}, vmNotFound(target, nil)
	}

	if strings.EqualFold(provisioningState(vm), "Deleting") {
		return machineops.BeginResult{}, providerConflict(fmt.Errorf("VM %s is being deleted", target.VMName))
	}

	// A model recorded in the middle of another operation could differ from
	// the VM that operation leaves behind, so wait for it. The engine calls
	// Begin again.
	if state := strings.ToLower(provisioningState(vm)); isSettling(state) {
		return machineops.BeginResult{}, fmt.Errorf("VM %s is %s; its replacement begins when that is done", target.VMName, state)
	}

	// The preparation is a write conditional on the etag, so a VM Azure
	// reports without one is refused while it is still intact.
	if toValue(vm.Etag, "") == "" {
		return machineops.BeginResult{}, unsupportedVM(target, errors.New("it reports no etag"))
	}

	vmID, replacement, err := replacementFor(vm, request)
	if err != nil {
		return machineops.BeginResult{}, unsupportedVM(target, err)
	}

	model, err := encodeReplacementModel(replacement)
	if err != nil {
		return machineops.BeginResult{}, unsupportedVM(target, err)
	}

	handle := replaceHandle{OriginalVMID: vmID, OriginalOSDiskID: originalOSDiskID(vm), Model: model}

	token, err := json.Marshal(handle)
	if err != nil {
		return machineops.BeginResult{}, fmt.Errorf("encode HostReplace resume token: %w", err)
	}

	return machineops.BeginResult{
		Operation: machineops.ProviderOperation{
			OperationID: target.id(),
			ResumeToken: string(token),
		},
		Message:      "recorded the model of VM " + target.VMName + " for its replacement",
		RequeueAfter: replaceStepInterval,
	}, nil
}

// replaceStepInterval is the wait before the Poll that follows a step Azure
// completes quickly, so the replacement does not idle for a whole poll
// interval between its steps.
const replaceStepInterval = pollInterval / 3

// isSettling reports a provisioning state in which Azure is still changing
// the VM, so its model is not final and a write would be refused.
func isSettling(state string) bool {
	return state == "creating" || state == "updating" || state == "migrating"
}

// replacementFor validates the observed VM and builds its replacement. It
// returns Azure's unique ID of the observed VM with it.
func replacementFor(vm *armcompute.VirtualMachine, request machineops.OperationRequest) (string, armcompute.VirtualMachine, error) {
	if err := validateReplaceable(vm, request.HostImage); err != nil {
		return "", armcompute.VirtualMachine{}, err
	}

	vmID := strings.TrimSpace(toValue(vm.Properties.VMID, ""))
	if vmID == "" {
		return "", armcompute.VirtualMachine{}, errors.New("it reports no unique ID")
	}

	restore, err := originalDeleteOptions(vm)
	if err != nil {
		return "", armcompute.VirtualMachine{}, err
	}

	replacement, err := prepareReplacementVM(*vm, restore, request.HostImage, request.OperationUID)
	if err != nil {
		return "", armcompute.VirtualMachine{}, err
	}

	return vmID, replacement, nil
}

// stillReplacedBy reports whether the VM would still be replaced by the
// model recorded in the handle. The model is fixed when Begin returns, so a
// VM that changed since then, for example by a disk attached in the
// meantime, must not be deleted: its replacement would silently undo the
// change. The models are compared as JSON, so a controller upgraded in the
// middle of an operation still compares like with like.
func stillReplacedBy(vm *armcompute.VirtualMachine, request machineops.OperationRequest, recorded armcompute.VirtualMachine) bool {
	_, current, err := replacementFor(vm, request)
	if err != nil {
		return false
	}

	want, err := json.Marshal(recorded)
	if err != nil {
		return false
	}

	got, err := json.Marshal(current)

	return err == nil && bytes.Equal(want, got)
}

func encodeReplacementModel(vm armcompute.VirtualMachine) (string, error) {
	raw, err := json.Marshal(vm)
	if err != nil {
		return "", fmt.Errorf("encode its model: %w", err)
	}

	var compressed bytes.Buffer

	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		return "", fmt.Errorf("compress its model: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("compress its model: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(compressed.Bytes())
	if len(raw) > replaceModelMaxBytes || len(encoded) > replaceModelMaxBytes {
		return "", fmt.Errorf("its model is too large to record in the MachineOperation status (limit %d bytes)", replaceModelMaxBytes)
	}

	return encoded, nil
}

func parseReplaceHandle(token string) (replaceHandle, armcompute.VirtualMachine, error) {
	var (
		handle replaceHandle
		model  armcompute.VirtualMachine
	)

	if err := json.Unmarshal([]byte(token), &handle); err != nil {
		return replaceHandle{}, model, fmt.Errorf("parse HostReplace resume token: %w", err)
	}

	if handle.OriginalVMID == "" || handle.Model == "" {
		return replaceHandle{}, model, errors.New("HostReplace resume token names no VM model")
	}

	compressed, err := base64.StdEncoding.DecodeString(handle.Model)
	if err != nil {
		return replaceHandle{}, model, fmt.Errorf("decode HostReplace VM model: %w", err)
	}

	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return replaceHandle{}, model, fmt.Errorf("decompress HostReplace VM model: %w", err)
	}

	// One byte over the limit is enough to tell a model that is too large.
	raw, err := io.ReadAll(io.LimitReader(reader, replaceModelMaxBytes+1))
	if err != nil {
		return replaceHandle{}, model, fmt.Errorf("decompress HostReplace VM model: %w", err)
	}

	if len(raw) > replaceModelMaxBytes {
		return replaceHandle{}, model, errors.New("HostReplace VM model exceeds its size limit")
	}

	if err := json.Unmarshal(raw, &model); err != nil {
		return replaceHandle{}, model, fmt.Errorf("parse HostReplace VM model: %w", err)
	}

	if model.Properties == nil || model.Properties.OSProfile == nil {
		return replaceHandle{}, model, errors.New("HostReplace VM model has no OS profile")
	}

	return handle, model, nil
}

var errReplaceUserDataUnavailable = errors.New("replacement user data is required for Azure VM HostReplace")

func validateReplaceUserData(userData string) error {
	if userData == "" {
		return errReplaceUserDataUnavailable
	}

	if err := validateAzureCustomData(userData); err != nil {
		return fmt.Errorf("validate Azure replacement custom data: %w", err)
	}

	return nil
}

func unsupportedVM(target azureVMResourceRef, err error) error {
	return &machineops.PermanentError{
		Reason: reasonUnsupportedVM,
		Err:    fmt.Errorf("VM %s cannot be replaced: %w", target.VMName, err),
	}
}

// PollReplace takes the next step of the replacement. It holds no state of
// its own: every call reads the VM and decides from what Azure reports and
// from the model in the handle, so a controller that restarts at any point
// continues where the last one stopped. Each call sends at most one request
// and does not wait for Azure to finish it.
//
//	VM Azure reports                          step
//	----------------------------------------  -----------------------------
//	the original, not prepared                prepare: set the delete options
//	                                          (OS disk Delete, NICs and data
//	                                          disks Detach) and tag it
//	the original, an update in progress       wait
//	the original, prepared                    delete it
//	the original, being deleted               wait
//	none                                      create the replacement
//	the replacement, being created            wait
//	the replacement, provisioned              done
//
// What protects a VM this operation should not touch:
//   - The original is prepared and deleted only while it is still the VM
//     Begin recorded and would still be replaced by the same model.
//   - The preparation is a write conditional on the VM's etag, and the create
//     is conditional on no VM having the name, so neither overwrites a VM
//     that changed or appeared after it was read.
//
// Two gaps remain, because Azure offers no conditional delete and Poll cannot
// record that it sent one: a VM that someone swaps for another between this
// Poll's read and its delete is deleted, and a VM that someone else deletes
// after Begin is created again as if this operation had deleted it.
//
// The model in the handle decides what is created once the VM is gone, and
// the handle is stored in the MachineOperation status. Whoever can write that
// status can therefore have a VM of their choosing created. The admission
// policy in deploy/machine-ops that reserves the handle for operations
// controllers is what closes this.
//
// An operation that ends while the original VM still exists gives up cleanly:
// the delete options and tags of the preparation are undone first (see
// giveUp).
//
// The engine builds the cloud-init again for every Poll, so the replacement
// gets the bootstrap token as it is when the VM is created, and the token
// reaches Azure as customData and nowhere else.
//
// Once the original VM is deleted the operation can only go forward. A
// failure Azure may recover from is retried for as long as it lasts, and the
// engine reports the operation as stalled. A create that Azure definitively
// refuses ends the operation with the VM gone; the NICs and data disks are
// left intact, and the model stays in the operation's status.
func (p *Provider) PollReplace(
	ctx context.Context,
	request machineops.OperationRequest,
	operation machineops.ProviderOperation,
) (machineops.PollResult, error) {
	target, err := hostTarget(request, &operation)
	if err != nil {
		return machineops.PollResult{}, invalidRequest(err)
	}

	uid := string(request.OperationUID)
	if strings.TrimSpace(uid) == "" {
		return machineops.PollResult{}, invalidRequest(errors.New("operation UID is required for Azure VM HostReplace"))
	}

	handle, model, err := parseReplaceHandle(operation.ResumeToken)
	if err != nil {
		return machineops.PollResult{}, invalidRequest(err)
	}

	client, err := p.client(target.SubscriptionID, request.Auth)
	if err != nil {
		return machineops.PollResult{}, fmt.Errorf("create Azure VM client: %w", err)
	}

	vm, err := client.GetVM(ctx, target.ResourceGroup, target.VMName, false)
	if err != nil {
		return machineops.PollResult{}, err
	}

	// Azure's unique ID tells the original from any VM created later under
	// its name. Tags alone do not: whoever can tag the original could mark it
	// as its own replacement.
	original := vm != nil && vm.Properties != nil && strings.EqualFold(toValue(vm.Properties.VMID, ""), handle.OriginalVMID)

	switch {
	case vm == nil:
		return createReplacement(ctx, client, target, model, request.ReplaceUserData)
	case original:
		return removeOriginal(ctx, client, target, vm, request, handle, model)
	case tagValue(vm.Tags, TagReplacedBy) == uid:
		return observeReplacement(ctx, client, target, vm)
	default:
		return hostFailed(string(unboundedv1alpha3.FailureProviderConflict),
			fmt.Sprintf("VM %s is neither the VM operation %s set out to replace nor its replacement", target.VMName, uid)), nil
	}
}

// removeOriginal prepares the original VM for deletion and deletes it.
func removeOriginal(
	ctx context.Context,
	client azureVMClient,
	target azureVMResourceRef,
	vm *armcompute.VirtualMachine,
	request machineops.OperationRequest,
	handle replaceHandle,
	model armcompute.VirtualMachine,
) (machineops.PollResult, error) {
	uid := string(request.OperationUID)
	pending := tagValue(vm.Tags, TagReplacePending) == uid

	switch state := strings.ToLower(provisioningState(vm)); {
	case state == "deleting":
		// This replacement's delete, or someone else's: either way the VM is
		// going, and it is created again once it is gone.
		return hostInProgress(fmt.Sprintf("waiting for Azure to delete VM %s", target.VMName)), nil
	case isSettling(state):
		// Wait for any operation in progress, including this replacement's
		// preparation, before the next write.
		return hostInProgress(fmt.Sprintf("waiting for VM %s to settle: provisioning state is %s", target.VMName, state)), nil
	}

	etag := toValue(vm.Etag, "")
	if etag == "" {
		return machineops.PollResult{}, fmt.Errorf("VM %s was returned without an etag, so it cannot be changed safely", target.VMName)
	}

	// Checked on the settled VM before each write, so the VM that is deleted
	// is the one the model describes.
	if !strings.EqualFold(originalOSDiskID(vm), handle.OriginalOSDiskID) || !stillReplacedBy(vm, request, model) {
		return giveUp(ctx, client, target, vm, uid, string(unboundedv1alpha3.FailureProviderConflict), fmt.Sprintf(
			"VM %s changed after operation %s recorded its model, so it was not deleted; a new HostReplace replaces it as it is now", target.VMName, uid,
		))
	}

	if !pending || !preparedForDelete(vm) {
		restore, err := originalDeleteOptions(vm)
		if err != nil {
			return machineops.PollResult{}, unsupportedVM(target, err)
		}

		err = client.UpdateVM(ctx, target.ResourceGroup, target.VMName, replacementDeletePayload(*vm, restore, uid), etag)
		if isPreconditionFailed(err) {
			return replaceStep(fmt.Sprintf("VM %s changed while it was prepared for replacement; reading it again", target.VMName)), nil
		}

		if err != nil {
			return giveUpOnPermanent(ctx, client, target, vm, uid, classifyActionError(target, "preparation for replacement", err))
		}

		return replaceStep(fmt.Sprintf("preparing VM %s for replacement", target.VMName)), nil
	}

	// Last check before the VM is destroyed: without usable cloud-init the
	// replacement could not be created. The engine sends none when it could
	// not build it, which may pass, so that is retried; cloud-init Azure
	// would refuse ends the operation while the VM still exists.
	if request.ReplaceUserData == "" {
		return machineops.PollResult{}, errReplaceUserDataUnavailable
	}

	if err := validateReplaceUserData(request.ReplaceUserData); err != nil {
		return giveUp(ctx, client, target, vm, uid, string(unboundedv1alpha3.FailureInvalidRequest), err.Error())
	}

	if err := client.DeleteVM(ctx, target.ResourceGroup, target.VMName); err != nil {
		return giveUpOnPermanent(ctx, client, target, vm, uid, classifyActionError(target, "delete for replacement", err))
	}

	return hostInProgress(fmt.Sprintf("deleting VM %s for replacement", target.VMName)), nil
}

// giveUpOnPermanent gives up when err ends the operation and returns every
// other error for the engine to retry.
func giveUpOnPermanent(
	ctx context.Context,
	client azureVMClient,
	target azureVMResourceRef,
	vm *armcompute.VirtualMachine,
	uid string,
	err error,
) (machineops.PollResult, error) {
	permanent, ok := errors.AsType[*machineops.PermanentError](err)
	if !ok {
		return machineops.PollResult{}, err
	}

	return giveUp(ctx, client, target, vm, uid, permanent.Reason, permanent.Error())
}

// giveUp ends the operation while the original VM still exists. If this
// operation prepared the VM, the preparation is undone first: left in place,
// it would delete the OS disk and keep the NICs and data disks the next time
// anyone deletes the VM, whatever its owner had chosen.
//
// The failure is reported only once Azure accepted the write that undoes the
// preparation. Until then Poll returns an error or reports progress, the
// engine calls it again, and it arrives here again.
func giveUp(
	ctx context.Context,
	client azureVMClient,
	target azureVMResourceRef,
	vm *armcompute.VirtualMachine,
	uid string,
	reason string,
	message string,
) (machineops.PollResult, error) {
	if tagValue(vm.Tags, TagReplacePending) != uid {
		return hostFailed(reason, message), nil
	}

	restore, err := parseReplaceRestore(tagValue(vm.Tags, TagReplaceRestore))
	if err != nil {
		return hostFailed(reason, fmt.Sprintf("%s; the delete options the replacement set on the VM could not be restored: %v", message, err)), nil
	}

	err = client.UpdateVM(ctx, target.ResourceGroup, target.VMName, undoPreparationPayload(*vm, restore), toValue(vm.Etag, ""))
	if isPreconditionFailed(err) {
		return replaceStep(fmt.Sprintf("VM %s changed while its preparation was undone; reading it again", target.VMName)), nil
	}

	if _, rejected := classifyCreateRejection(err); rejected {
		return hostFailed(reason, fmt.Sprintf("%s; the delete options the replacement set on the VM could not be restored: %v", message, err)), nil
	}

	if err != nil {
		return machineops.PollResult{}, fmt.Errorf("undo the preparation of VM %s: %w", target.VMName, err)
	}

	return hostFailed(reason, message), nil
}

// createReplacement creates the replacement once no VM has the name.
func createReplacement(
	ctx context.Context,
	client azureVMClient,
	target azureVMResourceRef,
	model armcompute.VirtualMachine,
	userData string,
) (machineops.PollResult, error) {
	// The original VM is gone, so an unusable cloud-init is retried instead
	// of ending the operation: the engine builds it again for the next Poll.
	if err := validateReplaceUserData(userData); err != nil {
		return machineops.PollResult{}, err
	}

	model.Properties.OSProfile.CustomData = new(base64.StdEncoding.EncodeToString([]byte(userData)))

	err := client.CreateVM(ctx, target.ResourceGroup, target.VMName, model)
	if isPreconditionFailed(err) {
		// A VM has the name after all: an earlier Poll's create that the read
		// above did not see yet, or someone else's. The next Poll tells which.
		return replaceStep(fmt.Sprintf("VM %s exists again; reading it", target.VMName)), nil
	}

	if rejection, ok := classifyCreateRejection(err); ok {
		return hostFailed(string(rejection.Reason), fmt.Sprintf(
			"VM %s no longer exists, and Azure rejected its replacement with error code %s (HTTP %d); recreate the VM manually from its detached NICs and data disks: %v",
			target.VMName, valueOrDefault(rejection.Code, "unknown"), rejection.Status, err,
		)), nil
	}

	if err != nil {
		return machineops.PollResult{}, fmt.Errorf("VM %s no longer exists and is not recreated yet: %w", target.VMName, err)
	}

	return hostInProgress(fmt.Sprintf("creating replacement VM %s", target.VMName)), nil
}

// observeReplacement reports how far Azure is with the replacement VM.
func observeReplacement(
	ctx context.Context,
	client azureVMClient,
	target azureVMResourceRef,
	vm *armcompute.VirtualMachine,
) (machineops.PollResult, error) {
	switch state := strings.ToLower(provisioningState(vm)); state {
	case "succeeded":
		return machineops.PollResult{
			State:   machineops.ProviderOperationStateSucceeded,
			Message: fmt.Sprintf("replacement VM %s is provisioned", target.VMName),
		}, nil
	case "failed", "canceled":
		withView, err := client.GetVM(ctx, target.ResourceGroup, target.VMName, true)
		if err != nil {
			return machineops.PollResult{}, err
		}

		if withView == nil {
			return hostFailed(reasonReplacementLost, fmt.Sprintf("replacement VM %s no longer exists", target.VMName)), nil
		}

		code, message := provisioningFailure(withView)

		return hostFailed(string(classifyAzureCode(code, message)), fmt.Sprintf("replacement VM %s provisioning failed: %s %s", target.VMName, valueOrDefault(code, state), message)), nil
	case "deleting":
		return hostFailed(reasonReplacementLost, fmt.Sprintf("replacement VM %s is being deleted", target.VMName)), nil
	default:
		return hostInProgress(fmt.Sprintf("replacement VM %s provisioning state is %s", target.VMName, valueOrDefault(state, "unknown"))), nil
	}
}

// replaceStep reports a step that Azure completes quickly.
func replaceStep(message string) machineops.PollResult {
	result := hostInProgress(message)
	result.RequeueAfter = replaceStepInterval

	return result
}

// isPreconditionFailed reports that Azure refused a conditional write
// because the VM is not in the state the write was conditional on.
func isPreconditionFailed(err error) bool {
	responseErr, ok := errors.AsType[*azcore.ResponseError](err)

	return ok && responseErr.StatusCode == http.StatusPreconditionFailed
}

// validateReplaceable refuses a VM the replacement cannot recreate from what
// Azure reports about it, before anything is changed. Azure never returns a
// VM's administrator password or its customData, and the replacement attaches
// existing NICs and managed data disks by ID.
func validateReplaceable(vm *armcompute.VirtualMachine, hostImage string) error {
	properties := vm.Properties
	if properties == nil || properties.StorageProfile == nil || properties.StorageProfile.OSDisk == nil {
		return errors.New("VM reports no OS disk storage profile")
	}

	storage := properties.StorageProfile

	if err := validateReplaceableOS(properties.OSProfile, storage.OSDisk); err != nil {
		return err
	}

	if storage.OSDisk.Vhd != nil {
		return errors.New("its OS disk is an unmanaged VHD")
	}

	if strings.TrimSpace(hostImage) == "" && !hasImageReference(storage.ImageReference) {
		return errors.New("it was not created from an image and the operation names no host image")
	}

	if err := validateReplaceableNetwork(properties.NetworkProfile); err != nil {
		return err
	}

	for _, disk := range storage.DataDisks {
		if err := validateReplaceableDataDisk(disk); err != nil {
			return err
		}
	}

	return nil
}

func validateReplaceableOS(profile *armcompute.OSProfile, osDisk *armcompute.OSDisk) error {
	if profile == nil {
		return errors.New("it has no OS profile to carry the replacement customData")
	}

	if profile.WindowsConfiguration != nil || (osDisk.OSType != nil && *osDisk.OSType == armcompute.OperatingSystemTypesWindows) {
		return errors.New("it runs Windows, whose replacement needs the administrator password Azure never returns")
	}

	linux := profile.LinuxConfiguration
	if linux == nil || linux.DisablePasswordAuthentication == nil || !*linux.DisablePasswordAuthentication {
		return errors.New("it allows password authentication, so its replacement needs the administrator password Azure never returns")
	}

	return nil
}

func validateReplaceableNetwork(network *armcompute.NetworkProfile) error {
	if network == nil {
		return errors.New("it has no network profile")
	}

	// A VM-managed NIC is created from the configuration, so recreating the
	// VM from it would make a new NIC with a new address and orphan the old one.
	if len(network.NetworkInterfaceConfigurations) > 0 {
		return errors.New("its network interfaces are created from network interface configurations and would not be reattached")
	}

	if len(network.NetworkInterfaces) == 0 {
		return errors.New("it references no network interface to reattach")
	}

	for i, nic := range network.NetworkInterfaces {
		if nic == nil || strings.TrimSpace(toValue(nic.ID, "")) == "" {
			return fmt.Errorf("network interface %d has no resource ID to reattach", i)
		}
	}

	return nil
}

func validateReplaceableDataDisk(disk *armcompute.DataDisk) error {
	if disk == nil || disk.Lun == nil {
		return errors.New("a data disk reports no LUN")
	}

	lun := *disk.Lun

	switch {
	case disk.Vhd != nil:
		return fmt.Errorf("the data disk at LUN %d is an unmanaged VHD and cannot be reattached", lun)
	case disk.ManagedDisk == nil || strings.TrimSpace(toValue(disk.ManagedDisk.ID, "")) == "":
		return fmt.Errorf("the data disk at LUN %d has no managed disk ID to reattach", lun)
	case disk.ToBeDetached != nil && *disk.ToBeDetached:
		return fmt.Errorf("the data disk at LUN %d is being detached", lun)
	}

	return nil
}

func hasImageReference(image *armcompute.ImageReference) bool {
	if image == nil {
		return false
	}

	if toValue(image.ID, "") != "" || toValue(image.SharedGalleryImageID, "") != "" || toValue(image.CommunityGalleryImageID, "") != "" {
		return true
	}

	return toValue(image.Publisher, "") != "" && toValue(image.Offer, "") != "" && toValue(image.SKU, "") != ""
}

// replaceRestore holds the delete options the VM had before the preparation
// set its OS disk to Delete and its NICs and data disks to Detach. A nil OS
// disk option is Azure's default; NICs and data disks not listed did not
// delete with the VM.
type replaceRestore struct {
	osDisk    *armcompute.DiskDeleteOptionTypes
	nicDelete []int
	lunDelete []int32
}

// originalDeleteOptions returns the VM's original delete options: from
// TagReplaceRestore once any replacement prepared the VM, whose current
// options are then the prepared ones, or from the VM itself.
func originalDeleteOptions(vm *armcompute.VirtualMachine) (replaceRestore, error) {
	if tagValue(vm.Tags, TagReplacePending) != "" {
		return recordedDeleteOptions(vm)
	}

	var restore replaceRestore

	if vm.Properties == nil {
		return restore, nil
	}

	if storage := vm.Properties.StorageProfile; storage != nil {
		if storage.OSDisk != nil && storage.OSDisk.DeleteOption != nil {
			restore.osDisk = new(*storage.OSDisk.DeleteOption)
		}

		for _, disk := range storage.DataDisks {
			if disk != nil && disk.Lun != nil && disk.DeleteOption != nil && *disk.DeleteOption == armcompute.DiskDeleteOptionTypesDelete {
				restore.lunDelete = append(restore.lunDelete, *disk.Lun)
			}
		}
	}

	if network := vm.Properties.NetworkProfile; network != nil {
		for i, nic := range network.NetworkInterfaces {
			if nic != nil && nic.Properties != nil && nic.Properties.DeleteOption != nil && *nic.Properties.DeleteOption == armcompute.DeleteOptionsDelete {
				restore.nicDelete = append(restore.nicDelete, i)
			}
		}
	}

	if len(restore.encode()) > azureTagValueMaxLength {
		return replaceRestore{}, fmt.Errorf("its delete options do not fit in Azure tag %s", TagReplaceRestore)
	}

	return restore, nil
}

// recordedDeleteOptions reads the restore record of a VM that a replacement
// prepared and did not finish, for example because its MachineOperation was
// deleted. The record is trusted only while the VM is exactly as the
// preparation left it: the record names NICs by position and data disks by
// LUN, and anyone who can tag the VM can write it.
func recordedDeleteOptions(vm *armcompute.VirtualMachine) (replaceRestore, error) {
	const remedy = "set the delete options of its OS disk, NICs and data disks and remove the tags " + TagReplacePending + " and " + TagReplaceRestore

	if !preparedForDelete(vm) {
		return replaceRestore{}, errors.New("an unfinished replacement left it tagged, and it changed since; " + remedy)
	}

	restore, err := parseReplaceRestore(tagValue(vm.Tags, TagReplaceRestore))
	if err != nil {
		return replaceRestore{}, fmt.Errorf("%w; %s", err, remedy)
	}

	nics := 0
	if vm.Properties.NetworkProfile != nil {
		nics = len(vm.Properties.NetworkProfile.NetworkInterfaces)
	}

	for _, index := range restore.nicDelete {
		if index >= nics {
			return replaceRestore{}, fmt.Errorf("tag %s names network interface %d, which it does not have; %s", TagReplaceRestore, index, remedy)
		}
	}

	for _, lun := range restore.lunDelete {
		if !slices.ContainsFunc(vm.Properties.StorageProfile.DataDisks, func(disk *armcompute.DataDisk) bool {
			return disk != nil && disk.Lun != nil && *disk.Lun == lun
		}) {
			return replaceRestore{}, fmt.Errorf("tag %s names a data disk at LUN %d, which it does not have; %s", TagReplaceRestore, lun, remedy)
		}
	}

	return restore, nil
}

// encode renders the restore record as "os=<option>;nic=<indexes>;lun=<LUNs>".
func (r replaceRestore) encode() string {
	osDisk := ""
	if r.osDisk != nil {
		osDisk = string(*r.osDisk)
	}

	nics := make([]string, 0, len(r.nicDelete))
	for _, index := range r.nicDelete {
		nics = append(nics, strconv.Itoa(index))
	}

	luns := make([]string, 0, len(r.lunDelete))
	for _, lun := range r.lunDelete {
		luns = append(luns, strconv.Itoa(int(lun)))
	}

	return "os=" + osDisk + ";nic=" + strings.Join(nics, ",") + ";lun=" + strings.Join(luns, ",")
}

func parseReplaceRestore(value string) (replaceRestore, error) {
	var restore replaceRestore

	fields := map[string]string{}

	for part := range strings.SplitSeq(value, ";") {
		key, field, ok := strings.Cut(part, "=")
		if !ok {
			return replaceRestore{}, fmt.Errorf("tag %s value %q is malformed", TagReplaceRestore, value)
		}

		fields[key] = field
	}

	osDisk, hasOS := fields["os"]
	nics, hasNIC := fields["nic"]
	luns, hasLUN := fields["lun"]

	if len(fields) != 3 || !hasOS || !hasNIC || !hasLUN {
		return replaceRestore{}, fmt.Errorf("tag %s value %q is malformed", TagReplaceRestore, value)
	}

	switch armcompute.DiskDeleteOptionTypes(osDisk) {
	case "":
	case armcompute.DiskDeleteOptionTypesDelete, armcompute.DiskDeleteOptionTypesDetach:
		restore.osDisk = new(armcompute.DiskDeleteOptionTypes(osDisk))
	default:
		return replaceRestore{}, fmt.Errorf("tag %s names unknown OS disk delete option %q", TagReplaceRestore, osDisk)
	}

	for index := range splitNonEmpty(nics) {
		parsed, err := strconv.Atoi(index)
		if err != nil || parsed < 0 {
			return replaceRestore{}, fmt.Errorf("tag %s names invalid network interface index %q", TagReplaceRestore, index)
		}

		restore.nicDelete = append(restore.nicDelete, parsed)
	}

	for lun := range splitNonEmpty(luns) {
		parsed, err := strconv.ParseInt(lun, 10, 32)
		if err != nil || parsed < 0 {
			return replaceRestore{}, fmt.Errorf("tag %s names invalid LUN %q", TagReplaceRestore, lun)
		}

		restore.lunDelete = append(restore.lunDelete, int32(parsed))
	}

	return restore, nil
}

// splitNonEmpty splits a comma-separated list, yielding nothing for "".
func splitNonEmpty(value string) iter.Seq[string] {
	if value == "" {
		return func(func(string) bool) {}
	}

	return strings.SplitSeq(value, ",")
}

// preparedForDelete reports that a delete of the VM keeps its NICs and data
// disks and removes its OS disk.
func preparedForDelete(vm *armcompute.VirtualMachine) bool {
	if vm.Properties == nil || vm.Properties.StorageProfile == nil || vm.Properties.StorageProfile.OSDisk == nil {
		return false
	}

	storage := vm.Properties.StorageProfile
	if storage.OSDisk.DeleteOption == nil || *storage.OSDisk.DeleteOption != armcompute.DiskDeleteOptionTypesDelete {
		return false
	}

	for _, disk := range storage.DataDisks {
		if disk == nil || disk.DeleteOption == nil || *disk.DeleteOption != armcompute.DiskDeleteOptionTypesDetach {
			return false
		}
	}

	if vm.Properties.NetworkProfile != nil {
		for _, nic := range vm.Properties.NetworkProfile.NetworkInterfaces {
			if nic == nil || nic.Properties == nil || nic.Properties.DeleteOption == nil || *nic.Properties.DeleteOption != armcompute.DeleteOptionsDetach {
				return false
			}
		}
	}

	return true
}

// replacementDeletePayload is the PUT that prepares the VM for deletion: the
// delete options of prepareVMForReplacementDelete and the tags that let a
// later Poll recognize the VM and a later replacement restore its original
// options.
func replacementDeletePayload(vm armcompute.VirtualMachine, restore replaceRestore, operationUID string) armcompute.VirtualMachine {
	payload := prepareVMForReplacementDelete(vm)
	payload.Tags = maps.Clone(vm.Tags)

	if payload.Tags == nil {
		payload.Tags = map[string]*string{}
	}

	setTag(payload.Tags, TagReplacePending, operationUID)
	setTag(payload.Tags, TagReplaceRestore, restore.encode())

	return payload
}

// undoPreparationPayload is the PUT that undoes replacementDeletePayload: the
// delete options the restore record names are set back and the two tags are
// removed. Everything the record does not name stays as it is now, so a NIC
// or disk attached since the preparation keeps its own option.
func undoPreparationPayload(vm armcompute.VirtualMachine, restore replaceRestore) armcompute.VirtualMachine {
	// The same cleaned copy the preparation starts from. It sets every NIC
	// and data disk to Detach and the OS disk to Delete; the loops below put
	// back what the VM has now and then what the record names.
	payload := prepareVMForReplacementDelete(vm)
	payload.Tags = make(map[string]*string, len(vm.Tags))

	for key, value := range vm.Tags {
		if !strings.EqualFold(key, TagReplacePending) && !strings.EqualFold(key, TagReplaceRestore) {
			payload.Tags[key] = value
		}
	}

	if payload.Properties == nil {
		return payload
	}

	if network, current := payload.Properties.NetworkProfile, vm.Properties.NetworkProfile; network != nil && current != nil {
		for i, nic := range network.NetworkInterfaces {
			if nic == nil || current.NetworkInterfaces[i] == nil {
				continue
			}

			nic.Properties = nil
			if properties := current.NetworkInterfaces[i].Properties; properties != nil {
				nic.Properties = new(*properties)
			}

			if slices.Contains(restore.nicDelete, i) {
				if nic.Properties == nil {
					nic.Properties = &armcompute.NetworkInterfaceReferenceProperties{}
				}

				nic.Properties.DeleteOption = new(armcompute.DeleteOptionsDelete)
			}
		}
	}

	if storage, current := payload.Properties.StorageProfile, vm.Properties.StorageProfile; storage != nil && current != nil {
		if storage.OSDisk != nil {
			storage.OSDisk.DeleteOption = restore.osDisk
		}

		for i, disk := range storage.DataDisks {
			if disk == nil || current.DataDisks[i] == nil {
				continue
			}

			disk.DeleteOption = current.DataDisks[i].DeleteOption
			if disk.Lun != nil && slices.Contains(restore.lunDelete, *disk.Lun) {
				disk.DeleteOption = new(armcompute.DiskDeleteOptionTypesDelete)
			}
		}
	}

	return payload
}

func prepareVMForReplacementDelete(vm armcompute.VirtualMachine) armcompute.VirtualMachine {
	// Azure SDK GET responses include read-only fields and child resources that
	// are invalid in a VM update payload. Clone the nested fields changed below
	// so preparing the independent replacement payload cannot alter this update.
	vm = cloneVMForReplacementMutation(vm)
	vm.ID = nil
	vm.Name = nil
	vm.Type = nil
	vm.Etag = nil
	vm.ManagedBy = nil
	vm.Resources = nil

	if vm.Properties == nil {
		return vm
	}

	vm.Properties.InstanceView = nil

	if vm.Properties.NetworkProfile != nil {
		for _, nic := range vm.Properties.NetworkProfile.NetworkInterfaces {
			if nic == nil {
				continue
			}

			if nic.Properties == nil {
				nic.Properties = &armcompute.NetworkInterfaceReferenceProperties{}
			}

			nic.Properties.DeleteOption = to.Ptr(armcompute.DeleteOptionsDetach)
		}
	}

	if vm.Properties.StorageProfile != nil {
		if vm.Properties.StorageProfile.OSDisk != nil {
			vm.Properties.StorageProfile.OSDisk.DeleteOption = to.Ptr(armcompute.DiskDeleteOptionTypesDelete)
		}

		for _, disk := range vm.Properties.StorageProfile.DataDisks {
			if disk != nil {
				disk.DeleteOption = to.Ptr(armcompute.DiskDeleteOptionTypesDetach)
			}
		}
	}

	return vm
}

// replacementOSDiskName derives the new OS disk name from the operation UID,
// so it differs from the disk being deleted and every Begin of one operation
// builds the same replacement.
func replacementOSDiskName(vmName string, operationUID types.UID) string {
	suffix := strings.ReplaceAll(string(operationUID), "-", "")
	suffix = "-osdisk-" + suffix[:min(len(suffix), 8)]

	return vmName[:min(len(vmName), azureDiskNameMaxLength-len(suffix))] + suffix
}

// prepareReplacementVM builds the replacement from the original VM's model.
// The OS disk is created again from the host image and named after the
// operation, the NICs and managed data disks are attached by ID and the
// original delete options are restored. The model carries no cloud-init:
// createReplacement adds it as customData, and never as userData, which
// Azure keeps readable through IMDS and ARM for the VM's whole life.
func prepareReplacementVM(
	vm armcompute.VirtualMachine,
	restore replaceRestore,
	hostImage string,
	operationUID types.UID,
) (armcompute.VirtualMachine, error) {
	// Clone the nested fields changed below so this create payload remains
	// independent from the pre-delete update payload built from the same source.
	vm = cloneVMForReplacementMutation(vm)

	imageReference, err := parseAzureImageReference(hostImage)
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}

	if imageReference != nil && (vm.Properties == nil || vm.Properties.StorageProfile == nil || vm.Properties.StorageProfile.OSDisk == nil) {
		return armcompute.VirtualMachine{}, fmt.Errorf("existing VM has no OS disk storage profile to replace the host image")
	}

	vmName := toValue(vm.Name, "replacement")
	vm.ID = nil
	vm.Name = nil
	vm.Type = nil
	vm.Etag = nil
	vm.ManagedBy = nil
	vm.Resources = nil
	vm.Tags = replacementTags(vm.Tags, string(operationUID))

	if vm.Identity != nil {
		for id := range vm.Identity.UserAssignedIdentities {
			vm.Identity.UserAssignedIdentities[id] = &armcompute.UserAssignedIdentitiesValue{}
		}
	}

	if vm.Properties == nil {
		return vm, nil
	}

	vm.Properties.ProvisioningState = nil
	vm.Properties.TimeCreated = nil
	vm.Properties.VMID = nil
	vm.Properties.InstanceView = nil
	// Never carry userData forward: a VM replaced by an earlier version of
	// this adapter holds an old bootstrap token there.
	vm.Properties.UserData = nil

	if vm.Properties.OSProfile != nil {
		vm.Properties.OSProfile.CustomData = nil
		vm.Properties.OSProfile.AdminPassword = nil
		vm.Properties.OSProfile.RequireGuestProvisionSignal = nil
	}

	if network := vm.Properties.NetworkProfile; network != nil {
		for i, nic := range network.NetworkInterfaces {
			if nic == nil {
				continue
			}

			if nic.Properties == nil {
				nic.Properties = &armcompute.NetworkInterfaceReferenceProperties{}
			}

			nic.Properties.DeleteOption = new(armcompute.DeleteOptionsDetach)
			if slices.Contains(restore.nicDelete, i) {
				nic.Properties.DeleteOption = new(armcompute.DeleteOptionsDelete)
			}
		}
	}

	storage := vm.Properties.StorageProfile
	if storage == nil {
		return vm, nil
	}

	if storage.OSDisk != nil {
		if imageReference != nil {
			storage.ImageReference = imageReference
		}

		storage.OSDisk.CreateOption = to.Ptr(armcompute.DiskCreateOptionTypesFromImage)
		storage.OSDisk.DeleteOption = restore.osDisk
		storage.OSDisk.Vhd = nil

		if storage.OSDisk.ManagedDisk != nil {
			storage.OSDisk.ManagedDisk.ID = nil
		}

		storage.OSDisk.Name = new(replacementOSDiskName(vmName, operationUID))
	}

	storage.DataDisks = attachedDataDisks(storage.DataDisks, restore)

	return vm, nil
}

// attachedDataDisks reattaches every surviving managed data disk by ID. A
// disk created with the original VM, for example with CreateOption Empty,
// already exists and must not be created again.
func attachedDataDisks(disks []*armcompute.DataDisk, restore replaceRestore) []*armcompute.DataDisk {
	if disks == nil {
		return nil
	}

	attached := make([]*armcompute.DataDisk, 0, len(disks))

	for _, disk := range disks {
		if disk == nil {
			continue
		}

		deleteOption := armcompute.DiskDeleteOptionTypesDetach
		if disk.Lun != nil && slices.Contains(restore.lunDelete, *disk.Lun) {
			deleteOption = armcompute.DiskDeleteOptionTypesDelete
		}

		var managedDisk *armcompute.ManagedDiskParameters
		if disk.ManagedDisk != nil {
			managedDisk = &armcompute.ManagedDiskParameters{ID: disk.ManagedDisk.ID}
		}

		attached = append(attached, &armcompute.DataDisk{
			Lun:                     disk.Lun,
			Name:                    disk.Name,
			CreateOption:            new(armcompute.DiskCreateOptionTypesAttach),
			ManagedDisk:             managedDisk,
			Caching:                 disk.Caching,
			WriteAcceleratorEnabled: disk.WriteAcceleratorEnabled,
			DeleteOption:            new(deleteOption),
		})
	}

	return attached
}

// replacementTags copies the VM's tags without the preparation tags and
// marks the replacement with the operation that creates it.
func replacementTags(tags map[string]*string, operationUID string) map[string]*string {
	replaced := make(map[string]*string, len(tags)+1)

	for key, value := range tags {
		if strings.EqualFold(key, TagReplacePending) || strings.EqualFold(key, TagReplaceRestore) || strings.EqualFold(key, TagReplacedBy) {
			continue
		}

		replaced[key] = value
	}

	replaced[TagReplacedBy] = new(operationUID)

	return replaced
}

// setTag sets a tag, replacing any key that differs only in case: Azure tag
// names are case-insensitive.
func setTag(tags map[string]*string, key, value string) {
	for existing := range tags {
		if strings.EqualFold(existing, key) {
			delete(tags, existing)
		}
	}

	tags[key] = new(value)
}

func cloneVMForReplacementMutation(vm armcompute.VirtualMachine) armcompute.VirtualMachine {
	cloned := vm

	if vm.Identity != nil {
		identity := *vm.Identity
		cloned.Identity = &identity

		if vm.Identity.UserAssignedIdentities != nil {
			identity.UserAssignedIdentities = maps.Clone(vm.Identity.UserAssignedIdentities)
		}
	}

	if vm.Properties == nil {
		return cloned
	}

	properties := *vm.Properties
	cloned.Properties = &properties

	if vm.Properties.OSProfile != nil {
		osProfile := *vm.Properties.OSProfile
		properties.OSProfile = &osProfile
	}

	if vm.Properties.NetworkProfile != nil {
		networkProfile := *vm.Properties.NetworkProfile
		properties.NetworkProfile = &networkProfile

		if vm.Properties.NetworkProfile.NetworkInterfaces != nil {
			networkProfile.NetworkInterfaces = make(
				[]*armcompute.NetworkInterfaceReference,
				len(vm.Properties.NetworkProfile.NetworkInterfaces),
			)
			for i, reference := range vm.Properties.NetworkProfile.NetworkInterfaces {
				if reference == nil {
					continue
				}

				clonedReference := *reference
				networkProfile.NetworkInterfaces[i] = &clonedReference

				if reference.Properties != nil {
					referenceProperties := *reference.Properties
					clonedReference.Properties = &referenceProperties
				}
			}
		}
	}

	if vm.Properties.StorageProfile != nil {
		storageProfile := *vm.Properties.StorageProfile
		properties.StorageProfile = &storageProfile

		if vm.Properties.StorageProfile.OSDisk != nil {
			osDisk := *vm.Properties.StorageProfile.OSDisk
			storageProfile.OSDisk = &osDisk

			if vm.Properties.StorageProfile.OSDisk.ManagedDisk != nil {
				managedDisk := *vm.Properties.StorageProfile.OSDisk.ManagedDisk
				osDisk.ManagedDisk = &managedDisk
			}
		}

		if vm.Properties.StorageProfile.DataDisks != nil {
			storageProfile.DataDisks = make(
				[]*armcompute.DataDisk,
				len(vm.Properties.StorageProfile.DataDisks),
			)
			for i, dataDisk := range vm.Properties.StorageProfile.DataDisks {
				if dataDisk == nil {
					continue
				}

				clonedDataDisk := *dataDisk
				storageProfile.DataDisks[i] = &clonedDataDisk
			}
		}
	}

	return cloned
}
