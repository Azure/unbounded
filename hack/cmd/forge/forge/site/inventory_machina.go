package site

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	machinav1alpha3 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha3"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/site/azuredev"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// sanitizeK8sName converts a raw string into a valid Kubernetes object name.
// Kubernetes names must be lowercase RFC-1123 subdomains: lowercase alphanumeric
// characters, '-' or '.', must start and end with an alphanumeric character,
// and be at most 253 characters.
func sanitizeK8sName(raw string) string {
	s := strings.ToLower(raw)

	// Replace any character that is not alphanumeric, '-', or '.' with '-'.
	re := regexp.MustCompile(`[^a-z0-9\-.]`)
	s = re.ReplaceAllString(s, "-")

	// Collapse consecutive dashes.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}

	// Trim leading/trailing non-alphanumeric characters.
	s = strings.TrimLeft(s, "-.")
	s = strings.TrimRight(s, "-.")

	// Truncate to 253 characters.
	if len(s) > 253 {
		s = s[:253]
		s = strings.TrimRight(s, "-.")
	}

	return s
}

// machinaNameFromInventory builds a Machine CR name from the LB IP and NAT port.
// For example, "20.48.100.5" port 50001 becomes "20.48.100.5-50001".
func machinaNameFromInventory(m azuredev.Machine) string {
	return sanitizeK8sName(fmt.Sprintf("%s-%d", m.IPAddress, m.Port))
}

// WriteInventoryAsMachina writes the inventory as a multi-document YAML stream
// of machina Machine custom resources that can be applied with kubectl.
func WriteInventoryAsMachina(w io.Writer, inventory *azuredev.Inventory) error {
	for i, m := range inventory.Machines {
		machine := machinav1alpha3.Machine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: machinav1alpha3.GroupVersion.String(),
				Kind:       "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: machinaNameFromInventory(m),
				Annotations: map[string]string{
					"forge.unbounded-kube.io/vm-name": m.Name,
				},
			},
			Spec: machinav1alpha3.MachineSpec{
				SSH: &machinav1alpha3.SSHSpec{
					Host: fmt.Sprintf("%s:%d", m.IPAddress, m.Port),
				},
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
