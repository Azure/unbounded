package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

type Inventory struct {
	VMs []InventoryVM `yaml:"vms"`
}

type InventoryVM struct {
	Name string `yaml:"name"`
	VMID int    `yaml:"vmid"`
	MAC  string `yaml:"mac"`
	IPv4 string `yaml:"ipv4"`

	IntendedIPv4 string `yaml:"intendedIPv4"`
}

type MachineRenderInput struct {
	Site                 string
	BootstrapTokenName   string
	RedfishURL           string
	RedfishUsername      string
	BMCSecretName        string
	BMCSecretNamespace   string
	BMCSecretKey         string
	Image                string
	Network              MachineNetworkInput
	InitialRebootCounter int
	InitialRepaveCounter int
}

type MachineNetworkInput struct {
	SubnetMask string
	Gateway    string
	DNS        []string
}

func ValidateMachineRenderInput(input MachineRenderInput) error {
	missing := []string{}
	if input.Site == "" {
		missing = append(missing, "site is required")
	}
	if input.BootstrapTokenName == "" {
		missing = append(missing, "bootstrapTokenName is required")
	}
	if input.RedfishURL == "" {
		missing = append(missing, "redfish.url is required")
	}
	if input.RedfishUsername == "" {
		missing = append(missing, "redfish.username is required")
	}
	if input.BMCSecretName == "" {
		missing = append(missing, "redfish.secretName is required")
	}
	if input.BMCSecretNamespace == "" {
		missing = append(missing, "redfish.secretNamespace is required")
	}
	if input.Image == "" {
		missing = append(missing, "pxeImage is required")
	}
	if input.Network.SubnetMask == "" {
		missing = append(missing, "network.subnetMask is required")
	}
	if input.Network.Gateway == "" {
		missing = append(missing, "network.gateway is required")
	}
	if len(missing) > 0 {
		return errors.New(strings.Join(missing, "; "))
	}
	return nil
}

func MachineRenderInputFromEnvironment(env EnvironmentFile, overrides RenderMachinesConfig) MachineRenderInput {
	input := MachineRenderInput{
		Site:                 env.Site,
		BootstrapTokenName:   env.BootstrapTokenName,
		RedfishURL:           env.Redfish.URL,
		RedfishUsername:      env.Redfish.Username,
		BMCSecretName:        env.Redfish.SecretName,
		BMCSecretNamespace:   env.Redfish.SecretNamespace,
		BMCSecretKey:         env.Redfish.SecretKey,
		Image:                env.PXEImage,
		InitialRebootCounter: env.InitialRebootCounter,
		InitialRepaveCounter: env.InitialRepaveCounter,
		Network: MachineNetworkInput{
			SubnetMask: env.Network.SubnetMask,
			Gateway:    env.Network.Gateway,
			DNS:        append([]string(nil), env.Network.DNS...),
		},
	}
	if overrides.PXEImage != "" {
		input.Image = overrides.PXEImage
	}
	if overrides.BootstrapToken != "" {
		input.BootstrapTokenName = overrides.BootstrapToken
	}
	if overrides.BMCSecretKey != "" {
		input.BMCSecretKey = overrides.BMCSecretKey
	}
	return input
}

const machineTemplate = `{{- range .VMs }}
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: {{ .Name }}
  labels:
    unbounded-kube.io/site: {{ $.Input.Site }}
spec:
  kubernetes:
    bootstrapTokenRef:
      name: {{ $.Input.BootstrapTokenName }}
    version: v1.34.4
  operations:
    rebootCounter: {{ $.Input.InitialRebootCounter }}
    repaveCounter: {{ $.Input.InitialRepaveCounter }}
  pxe:
    image: {{ $.Input.Image }}
    dhcpLeases:
      - ipv4: {{ .IPv4 | printf "%q" }}
        mac: {{ .MAC | printf "%q" }}
        subnetMask: {{ $.Input.Network.SubnetMask | printf "%q" }}
        gateway: {{ $.Input.Network.Gateway | printf "%q" }}
        dns: {{ $.Input.Network.DNS | toYAMLInline }}
    redfish:
      url: {{ $.Input.RedfishURL }}
      username: {{ $.Input.RedfishUsername }}
      deviceID: {{ .VMID | quoteVMID }}
      passwordRef:
        name: {{ $.Input.BMCSecretName }}
        namespace: {{ $.Input.BMCSecretNamespace }}
        key: {{ $.Input.BMCSecretKey | bmcSecretKey .Name }}
---
{{ end -}}`

func ParseInventory(data []byte) (Inventory, error) {
	var inv Inventory
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return Inventory{}, err
	}
	if len(inv.VMs) == 0 {
		return Inventory{}, fmt.Errorf("inventory must contain at least one VM")
	}
	for i, vm := range inv.VMs {
		if vm.VMID <= 0 {
			return Inventory{}, fmt.Errorf("vm %d: vmid must be greater than zero", i)
		}
		if vm.Name == "" {
			return Inventory{}, fmt.Errorf("vm %d: name is required", i)
		}
		inv.VMs[i] = normalizeInventoryVM(vm)
		vm = inv.VMs[i]
		if vm.MAC == "" {
			return Inventory{}, fmt.Errorf("vm %d: mac is required", i)
		}
		if vm.IPv4 == "" {
			return Inventory{}, fmt.Errorf("vm %d: ipv4 is required", i)
		}
	}
	return inv, nil
}

func normalizeInventoryVM(vm InventoryVM) InventoryVM {
	if vm.IPv4 == "" {
		vm.IPv4 = vm.IntendedIPv4
	}
	if vm.IPv4 == "" {
		if index, ok := stretchPXEIndex(vm.Name); ok {
			vm.IPv4 = fmt.Sprintf("10.10.100.%d", 50+index)
		}
	}
	if vm.MAC != "" && !strings.Contains(vm.MAC, ":") {
		vm.MAC = "02:10:10:64:00:" + vm.MAC
	}
	return vm
}

func stretchPXEIndex(name string) (int, bool) {
	const prefix = "stretch-pxe-"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func RenderMachines(inv Inventory, input MachineRenderInput) (string, error) {
	if len(inv.VMs) == 0 {
		return "", fmt.Errorf("inventory must contain at least one VM")
	}
	if err := ValidateMachineRenderInput(input); err != nil {
		return "", err
	}

	buf := &bytes.Buffer{}
	tmpl := template.Must(template.New("machines").Funcs(template.FuncMap{
		"quoteVMID": func(vmid int) string {
			return strconv.Quote(strconv.Itoa(vmid))
		},
		"toYAMLInline": func(values []string) string {
			quoted := make([]string, 0, len(values))
			for _, value := range values {
				quoted = append(quoted, strconv.Quote(value))
			}
			return "[" + strings.Join(quoted, ", ") + "]"
		},
		"bmcSecretKey": func(machineName, explicitKey string) string {
			if explicitKey != "" {
				return explicitKey
			}
			return machineName
		},
	}).Parse(machineTemplate))
	if err := tmpl.Execute(buf, map[string]any{"VMs": inv.VMs, "Input": input}); err != nil {
		return "", err
	}

	return buf.String(), nil
}
