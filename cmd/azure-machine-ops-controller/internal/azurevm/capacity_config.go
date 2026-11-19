// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Provider configuration keys read from the MachineClass providerConfigRef
// ConfigMap. Unknown keys are rejected so a typo never silently changes the
// machines a class launches.
//
// maxPods, kubeReserved and systemReserved do not configure the kubelet. This
// adapter only publishes them in the class's offering, as the pods capacity
// and the reserved overhead a consumer sizes a Node with. They DESCRIBE what
// the operator's machine configuration makes the kubelet do, and they must
// match it: setting maxPods to 250 while the kubelet keeps its default of 110
// publishes capacity the Node does not have. Leave them unset only when the
// kubelet runs with its defaults (110 pods, no reservations).
const (
	ConfigSubscriptionID           = "subscriptionID"
	ConfigResourceGroup            = "resourceGroup"
	ConfigLocation                 = "location"
	ConfigZone                     = "zone"
	ConfigVMSize                   = "vmSize"
	ConfigSubnetID                 = "subnetID"
	ConfigImage                    = "image"
	ConfigAdminUsername            = "adminUsername"
	ConfigSSHPublicKey             = "sshPublicKey"
	ConfigWorkerIdentityResourceID = "workerIdentityResourceID"
	ConfigOSDiskSizeGB             = "osDiskSizeGB"
	ConfigOSDiskType               = "osDiskType"
	ConfigMaxPods                  = "maxPods"
	ConfigMemoryOverheadPercent    = "memoryOverheadPercent"
	ConfigKubeReserved             = "kubeReserved"
	ConfigSystemReserved           = "systemReserved"
	ConfigPriceCurrency            = "priceCurrency"
	ConfigPricePerHour             = "pricePerHour"
	ConfigPriceSource              = "priceSource"
	ConfigPriceObservedAt          = "priceObservedAt"
)

const (
	defaultOSDiskSizeGB          = 64
	defaultOSDiskType            = "Premium_LRS"
	defaultMaxPods               = 110
	defaultMemoryOverheadPercent = 7.5
	maxOSDiskSizeGB              = 4095
	maxPodsLimit                 = 250
)

var (
	requiredConfigKeys = []string{
		ConfigSubscriptionID,
		ConfigResourceGroup,
		ConfigLocation,
		ConfigVMSize,
		ConfigSubnetID,
		ConfigImage,
		ConfigAdminUsername,
		ConfigSSHPublicKey,
	}

	optionalConfigKeys = []string{
		// A user-assigned identity is attached to the VM only when one is
		// named, for example to let the host read a private agent artifact.
		ConfigWorkerIdentityResourceID,
		ConfigZone,
		ConfigOSDiskSizeGB,
		ConfigOSDiskType,
		ConfigMaxPods,
		ConfigMemoryOverheadPercent,
		ConfigKubeReserved,
		ConfigSystemReserved,
		ConfigPriceCurrency,
		ConfigPricePerHour,
		ConfigPriceSource,
		ConfigPriceObservedAt,
	}

	supportedOSDiskTypes = []string{
		"Premium_LRS",
		"Premium_ZRS",
		"StandardSSD_LRS",
		"StandardSSD_ZRS",
		"Standard_LRS",
	}

	reservableResources = []corev1.ResourceName{
		corev1.ResourceCPU,
		corev1.ResourceMemory,
		corev1.ResourceEphemeralStorage,
		corev1.ResourcePods,
	}

	locationPattern = regexp.MustCompile(`^[a-z0-9]+$`)
	zonePattern     = regexp.MustCompile(`^[1-9][0-9]*$`)
	// Constrained-core sizes such as Standard_E4-2ads_v5 contain a hyphen.
	vmSizePattern        = regexp.MustCompile(`^[A-Za-z0-9]+(?:[_-][A-Za-z0-9]+)*$`)
	subscriptionPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	resourceGroupPattern = regexp.MustCompile(`^[\p{L}\p{N}_.()-]{1,90}$`)
	imageVersionPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	adminUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	pricePattern         = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	currencyPattern      = regexp.MustCompile(`^[A-Z]{3}$`)
)

// reservedAdminUsernames are the Linux administrator names Azure refuses in a
// VM create.
var reservedAdminUsernames = []string{
	"1", "123", "a", "actuser", "adm", "admin", "admin1", "admin2", "administrator",
	"aspnet", "backup", "console", "david", "guest", "john", "owner", "root", "server",
	"sql", "support", "support_388945a0", "sys", "test", "test1", "test2", "test3",
	"user", "user1", "user2", "user3", "user4", "user5",
}

// capacityConfig is the validated AzureVM provider configuration for one
// MachineClass.
type capacityConfig struct {
	SubscriptionID           string
	ResourceGroup            string
	Location                 string
	Zone                     string
	VMSize                   string
	SubnetID                 string
	Image                    string
	AdminUsername            string
	SSHPublicKey             string
	WorkerIdentityResourceID string
	OSDiskSizeGB             int32
	OSDiskType               string
	MemoryOverheadPercent    float64
	// MaxPods, KubeReserved and SystemReserved are published in the offering
	// and are never applied to the host. They must describe the kubelet
	// settings the operator's machine configuration applies.
	MaxPods        int64
	KubeReserved   corev1.ResourceList
	SystemReserved corev1.ResourceList
	Price          *configuredPrice
}

type configuredPrice struct {
	Currency   string
	PerHour    string
	Source     string
	ObservedAt time.Time
}

func parseCapacityConfig(data map[string]string) (capacityConfig, error) {
	if err := validateConfigKeys(data); err != nil {
		return capacityConfig{}, err
	}

	config := capacityConfig{
		SubscriptionID:           strings.TrimSpace(data[ConfigSubscriptionID]),
		ResourceGroup:            strings.TrimSpace(data[ConfigResourceGroup]),
		Location:                 strings.TrimSpace(data[ConfigLocation]),
		Zone:                     strings.TrimSpace(data[ConfigZone]),
		VMSize:                   strings.TrimSpace(data[ConfigVMSize]),
		SubnetID:                 strings.TrimSpace(data[ConfigSubnetID]),
		Image:                    strings.TrimSpace(data[ConfigImage]),
		AdminUsername:            strings.TrimSpace(data[ConfigAdminUsername]),
		SSHPublicKey:             strings.TrimSpace(data[ConfigSSHPublicKey]),
		WorkerIdentityResourceID: strings.TrimSpace(data[ConfigWorkerIdentityResourceID]),
		OSDiskType:               valueOrDefault(data[ConfigOSDiskType], defaultOSDiskType),
	}

	if err := validateIdentityFields(config); err != nil {
		return capacityConfig{}, err
	}

	if err := parseSizingFields(data, &config); err != nil {
		return capacityConfig{}, err
	}

	price, err := parseConfiguredPrice(data)
	if err != nil {
		return capacityConfig{}, err
	}

	config.Price = price

	return config, nil
}

func validateConfigKeys(data map[string]string) error {
	for key := range data {
		if !slices.Contains(requiredConfigKeys, key) && !slices.Contains(optionalConfigKeys, key) {
			return fmt.Errorf("unknown AzureVM provider config key %q", key)
		}
	}

	for _, key := range requiredConfigKeys {
		if strings.TrimSpace(data[key]) == "" {
			return fmt.Errorf("AzureVM provider config key %q is required", key)
		}
	}

	return nil
}

func validateIdentityFields(config capacityConfig) error {
	// Both become path segments of the VM's ARM ID, so a value Azure would
	// refuse must not reach the planned providerID.
	if !subscriptionPattern.MatchString(config.SubscriptionID) {
		return fmt.Errorf("%s %q must be a subscription GUID", ConfigSubscriptionID, config.SubscriptionID)
	}

	if !resourceGroupPattern.MatchString(config.ResourceGroup) || strings.HasSuffix(config.ResourceGroup, ".") {
		return fmt.Errorf("%s %q is not a valid Azure resource group name", ConfigResourceGroup, config.ResourceGroup)
	}

	if !locationPattern.MatchString(config.Location) {
		return fmt.Errorf("%s %q must be a lowercase Azure region name", ConfigLocation, config.Location)
	}

	if config.Zone != "" && !zonePattern.MatchString(config.Zone) {
		return fmt.Errorf("%s %q must be an Azure availability zone number", ConfigZone, config.Zone)
	}

	if !vmSizePattern.MatchString(config.VMSize) {
		return fmt.Errorf("%s %q is not a valid Azure VM size", ConfigVMSize, config.VMSize)
	}

	if err := validateResourceID(config.SubnetID, "Microsoft.Network/virtualNetworks", "subnets"); err != nil {
		return fmt.Errorf("%s: %w", ConfigSubnetID, err)
	}

	if config.WorkerIdentityResourceID != "" {
		if err := validateResourceID(config.WorkerIdentityResourceID, "Microsoft.ManagedIdentity/userAssignedIdentities", ""); err != nil {
			return fmt.Errorf("%s: %w", ConfigWorkerIdentityResourceID, err)
		}
	}

	if err := validatePinnedImage(config.Image); err != nil {
		return fmt.Errorf("%s: %w", ConfigImage, err)
	}

	if !adminUsernamePattern.MatchString(config.AdminUsername) {
		return fmt.Errorf("%s %q is not a valid Linux user name", ConfigAdminUsername, config.AdminUsername)
	}

	if slices.Contains(reservedAdminUsernames, config.AdminUsername) {
		return fmt.Errorf("%s %q is a reserved name Azure does not accept", ConfigAdminUsername, config.AdminUsername)
	}

	if !strings.HasPrefix(config.SSHPublicKey, "ssh-") && !strings.HasPrefix(config.SSHPublicKey, "ecdsa-") {
		return fmt.Errorf("%s must be an OpenSSH public key", ConfigSSHPublicKey)
	}

	// The key is written to authorized_keys as is, where a line break would
	// start another entry.
	if strings.ContainsAny(config.SSHPublicKey, "\r\n") {
		return fmt.Errorf("%s must be one OpenSSH public key on one line", ConfigSSHPublicKey)
	}

	if !slices.Contains(supportedOSDiskTypes, config.OSDiskType) {
		return fmt.Errorf("%s %q must be one of %s", ConfigOSDiskType, config.OSDiskType, strings.Join(supportedOSDiskTypes, ", "))
	}

	return nil
}

// validateResourceID checks that id is an ARM ID of providerType, optionally
// followed by one child collection such as subnets. The ID is sent to Azure
// verbatim, and the worker identity is a map key Azure matches exactly, so a
// trailing slash is rejected rather than tolerated.
func validateResourceID(id, providerType, childType string) error {
	if strings.HasSuffix(id, "/") {
		return fmt.Errorf("%q must not end with a slash", id)
	}

	parts := strings.Split(strings.Trim(id, "/"), "/")
	typeParts := strings.Split(providerType, "/")

	// subscriptions/{id}/resourceGroups/{rg}/providers, the provider type
	// segments, and the resource name.
	expected := 5 + len(typeParts) + 1
	if childType != "" {
		expected += 2
	}

	if !strings.HasPrefix(id, "/") || len(parts) != expected {
		return fmt.Errorf("%q must be a %s resource ID", id, strings.Trim(providerType+"/"+childType, "/"))
	}

	if !strings.EqualFold(parts[0], "subscriptions") ||
		!strings.EqualFold(parts[2], "resourceGroups") ||
		!strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], typeParts[0]) ||
		!strings.EqualFold(parts[6], typeParts[1]) {
		return fmt.Errorf("%q must be a %s resource ID", id, providerType)
	}

	if childType != "" && !strings.EqualFold(parts[8], childType) {
		return fmt.Errorf("%q must be a %s/%s resource ID", id, providerType, childType)
	}

	if slices.Contains(parts, "") {
		return fmt.Errorf("%q has empty path segments", id)
	}

	return nil
}

// validatePinnedImage requires an exact image version so every allocation of
// a class revision boots the same image. A marketplace reference must name a
// version other than latest. An image resource ID must be either a managed
// image, which has no versions, or a gallery image version: a gallery image
// ID without a version, or with the version latest, resolves to whatever
// version is newest at launch.
func validatePinnedImage(image string) error {
	reference, err := parseAzureImageReference(image)
	if err != nil {
		return err
	}

	if reference.ID != nil {
		return validatePinnedImageID(*reference.ID)
	}

	if reference.Version != nil && strings.EqualFold(*reference.Version, "latest") {
		return fmt.Errorf("image version must be pinned, not latest")
	}

	return nil
}

func validatePinnedImageID(id string) error {
	if strings.HasSuffix(id, "/") {
		return fmt.Errorf("image resource ID %q must not end with a slash", id)
	}

	parts := strings.Split(strings.TrimPrefix(id, "/"), "/")
	if slices.Contains(parts, "") {
		return fmt.Errorf("image resource ID %q has empty path segments", id)
	}

	// subscriptions/{id}/resourceGroups/{rg}/providers/Microsoft.Compute and
	// then the image path.
	if len(parts) < 6 ||
		!strings.EqualFold(parts[0], "subscriptions") ||
		!strings.EqualFold(parts[2], "resourceGroups") ||
		!strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], "Microsoft.Compute") {
		return fmt.Errorf("image resource ID %q must identify a Microsoft.Compute image or gallery image version", id)
	}

	path := parts[6:]

	switch {
	case len(path) == 2 && strings.EqualFold(path[0], "images"):
		return nil
	case len(path) == 6 && strings.EqualFold(path[0], "galleries") && strings.EqualFold(path[2], "images") && strings.EqualFold(path[4], "versions"):
		if !imageVersionPattern.MatchString(path[5]) {
			return fmt.Errorf("gallery image version %q must be pinned as major.minor.patch, not latest", path[5])
		}

		return nil
	case len(path) == 4 && strings.EqualFold(path[0], "galleries") && strings.EqualFold(path[2], "images"):
		return fmt.Errorf("gallery image ID %q must end with /versions/<major.minor.patch> so the image is pinned", id)
	default:
		return fmt.Errorf("image resource ID %q must identify a Microsoft.Compute image or gallery image version", id)
	}
}

func parseSizingFields(data map[string]string, config *capacityConfig) error {
	diskSize, err := parseBoundedInt(data[ConfigOSDiskSizeGB], defaultOSDiskSizeGB, 30, maxOSDiskSizeGB)
	if err != nil {
		return fmt.Errorf("%s: %w", ConfigOSDiskSizeGB, err)
	}

	maxPods, err := parseBoundedInt(data[ConfigMaxPods], defaultMaxPods, 10, maxPodsLimit)
	if err != nil {
		return fmt.Errorf("%s: %w", ConfigMaxPods, err)
	}

	overhead, err := parseOverheadPercent(data[ConfigMemoryOverheadPercent])
	if err != nil {
		return fmt.Errorf("%s: %w", ConfigMemoryOverheadPercent, err)
	}

	kubeReserved, err := parseReservedResources(data[ConfigKubeReserved])
	if err != nil {
		return fmt.Errorf("%s: %w", ConfigKubeReserved, err)
	}

	systemReserved, err := parseReservedResources(data[ConfigSystemReserved])
	if err != nil {
		return fmt.Errorf("%s: %w", ConfigSystemReserved, err)
	}

	config.OSDiskSizeGB = int32(diskSize)
	config.MaxPods = maxPods
	config.MemoryOverheadPercent = overhead
	config.KubeReserved = kubeReserved
	config.SystemReserved = systemReserved

	return nil
}

func parseBoundedInt(raw string, fallback, minimum, maximum int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}

	// Every bounded setting fits in 32 bits, and one is stored as an int32.
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q must be an integer between %d and %d", raw, minimum, maximum)
	}

	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%d must be between %d and %d", value, minimum, maximum)
	}

	return value, nil
}

func parseOverheadPercent(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultMemoryOverheadPercent, nil
	}

	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || value < 0 || value >= 50 {
		return 0, fmt.Errorf("%q must be a percentage in [0, 50)", raw)
	}

	return value, nil
}

// parseReservedResources parses kubelet-style "cpu=100m,memory=1Gi" lists.
// The format matches the kubelet flags because the values must repeat what
// the kubelet is configured with; parsing them here configures nothing.
func parseReservedResources(raw string) (corev1.ResourceList, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	resources := corev1.ResourceList{}

	for entry := range strings.SplitSeq(raw, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found {
			return nil, fmt.Errorf("entry %q must be name=quantity", entry)
		}

		resourceName := corev1.ResourceName(strings.TrimSpace(name))
		if !slices.Contains(reservableResources, resourceName) {
			return nil, fmt.Errorf("resource %q cannot be reserved", resourceName)
		}

		if _, exists := resources[resourceName]; exists {
			return nil, fmt.Errorf("resource %q is listed twice", resourceName)
		}

		quantity, err := resource.ParseQuantity(strings.TrimSpace(value))
		if err != nil || quantity.Sign() < 0 {
			return nil, fmt.Errorf("resource %q quantity %q is invalid", resourceName, value)
		}

		resources[resourceName] = quantity
	}

	return resources, nil
}

// parseConfiguredPrice reads an operator-supplied price observation. A price
// is optional, but when present it must say where and when it was observed so
// consumers never treat a stale list price as current.
func parseConfiguredPrice(data map[string]string) (*configuredPrice, error) {
	keys := []string{
		ConfigPriceCurrency,
		ConfigPricePerHour,
		ConfigPriceSource,
		ConfigPriceObservedAt,
	}

	present := 0

	for _, key := range keys {
		if strings.TrimSpace(data[key]) != "" {
			present++
		}
	}

	if present == 0 {
		return nil, nil
	}

	if present != len(keys) {
		slices.Sort(keys)
		return nil, fmt.Errorf("price keys %s must be set together", strings.Join(keys, ", "))
	}

	price := &configuredPrice{
		Currency: strings.TrimSpace(data[ConfigPriceCurrency]),
		PerHour:  strings.TrimSpace(data[ConfigPricePerHour]),
		Source:   strings.TrimSpace(data[ConfigPriceSource]),
	}

	if !currencyPattern.MatchString(price.Currency) {
		return nil, fmt.Errorf("%s %q must be an ISO 4217 code", ConfigPriceCurrency, price.Currency)
	}

	if !pricePattern.MatchString(price.PerHour) {
		return nil, fmt.Errorf("%s %q must be a non-negative decimal", ConfigPricePerHour, price.PerHour)
	}

	observedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(data[ConfigPriceObservedAt]))
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC 3339 timestamp: %w", ConfigPriceObservedAt, err)
	}

	price.ObservedAt = observedAt

	return price, nil
}

// offeringZone returns the offering zone, which is also the Node's
// topology.kubernetes.io/zone label. A configured Azure zone uses the
// region-number form Azure cloud provider Nodes use. Without a zone the VM is
// regional and the offering zone is the region name, so every offering has a
// zone a consumer can schedule against.
func (c capacityConfig) offeringZone() string {
	if c.Zone == "" {
		return c.Location
	}

	return c.Location + "-" + c.Zone
}

func valueOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}

	return value
}
