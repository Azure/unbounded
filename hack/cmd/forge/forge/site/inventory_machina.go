// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package site

import (
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	machinav1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/site/azuredev"
	"github.com/Azure/unbounded-kube/internal/kube"
)

// machinaNameFromInventory builds a Machine CR name from the site name, LB IP,
// and NAT port. For example, site "dc1" with IP "20.48.100.5" port 50001
// becomes "dc1-20.48.100.5-50001".
func machinaNameFromInventory(site string, m azuredev.Machine) string {
	return kube.SanitizeK8sName(fmt.Sprintf("%s-%s-%d", site, m.IPAddress, m.Port))
}

// parseSecretKeyRef parses a secret reference string in the format
// "[$namespace/]$secret-name[:$key]" into a SecretKeySelector.
// When $namespace/ is omitted, it defaults to "machina-system".
// When :$key is omitted, the Key field is left empty (relying on kubebuilder
// defaults).
func parseSecretKeyRef(ref string) (machinav1alpha3.SecretKeySelector, error) {
	if ref == "" {
		return machinav1alpha3.SecretKeySelector{}, fmt.Errorf("empty secret reference")
	}

	var sel machinav1alpha3.SecretKeySelector

	// Split off the optional :key suffix.
	namespaceName := ref

	if idx := strings.LastIndex(ref, ":"); idx >= 0 {
		sel.Key = ref[idx+1:]
		namespaceName = ref[:idx]
	}

	// Split the remainder into optional namespace/name.
	if idx := strings.Index(namespaceName, "/"); idx >= 0 {
		sel.Namespace = namespaceName[:idx]
		sel.Name = namespaceName[idx+1:]
	} else {
		sel.Namespace = "machina-system"
		sel.Name = namespaceName
	}

	if sel.Name == "" {
		return machinav1alpha3.SecretKeySelector{}, fmt.Errorf("empty secret name in reference %q", ref)
	}

	return sel, nil
}

// machineHost formats an IP address and port into a host string suitable for
// the Machine CR's spec.ssh.host field. When the port is the SSH default (22)
// or zero it is omitted; otherwise it is appended as "ip:port".
func machineHost(ip string, port int) string {
	if port != 0 && port != 22 {
		return fmt.Sprintf("%s:%d", ip, port)
	}

	return ip
}

// MachinaInventoryOptions holds optional parameters for WriteInventoryAsMachina.
type MachinaInventoryOptions struct {
	// Site is the site name used as a prefix for Machine CR names to avoid
	// collisions across sites.
	Site string

	// BastionHost, when non-empty (e.g. "1.2.3.4" or "1.2.3.4:2222"), causes
	// each Machine CR to include a spec.ssh.bastion entry pointing at the
	// bastion jump host.
	BastionHost string

	// SSHSecretRef, when non-nil, sets spec.ssh.privateKeyRef on each Machine.
	SSHSecretRef *machinav1alpha3.SecretKeySelector

	// BastionSSHSecretRef, when non-nil, sets spec.ssh.bastion.privateKeyRef
	// on each Machine. Only effective when BastionHost is also set.
	BastionSSHSecretRef *machinav1alpha3.SecretKeySelector

	// SSHUsername sets spec.ssh.username on each Machine CR.
	SSHUsername string

	// BastionSSHUsername sets spec.ssh.bastion.username on each Machine CR.
	// Only effective when BastionHost is also set.
	BastionSSHUsername string
}

// WriteInventoryAsMachina writes the inventory as a multi-document YAML stream
// of machina Machine custom resources that can be applied with kubectl.
func WriteInventoryAsMachina(w io.Writer, inventory *azuredev.Inventory, opts MachinaInventoryOptions) error {
	for i, m := range inventory.Machines {
		sshSpec := &machinav1alpha3.SSHSpec{
			Host:     machineHost(m.IPAddress, m.Port),
			Username: opts.SSHUsername,
		}

		if opts.SSHSecretRef != nil {
			sshSpec.PrivateKeyRef = *opts.SSHSecretRef
		}

		if opts.BastionHost != "" {
			bastion := &machinav1alpha3.BastionSSHSpec{
				Host:     opts.BastionHost,
				Username: opts.BastionSSHUsername,
			}

			if opts.BastionSSHSecretRef != nil {
				bastion.PrivateKeyRef = opts.BastionSSHSecretRef
			}

			sshSpec.Bastion = bastion
		}

		machine := machinav1alpha3.Machine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: machinav1alpha3.GroupVersion.String(),
				Kind:       "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: machinaNameFromInventory(opts.Site, m),
			},
			Spec: machinav1alpha3.MachineSpec{
				SSH: sshSpec,
			},
		}

		data, err := yaml.Marshal(machine)
		if err != nil {
			return fmt.Errorf("marshal machine %s: %w", m.Name, err)
		}

		if i > 0 {
			if _, err := fmt.Fprint(w, "---\n"); err != nil {
				return err
			}
		}

		if _, err := w.Write(data); err != nil {
			return err
		}
	}

	return nil
}
