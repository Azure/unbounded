package site

import (
	"fmt"
	"io"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/site/azuredev"
)

// WriteInventoryAsSSH writes an SSH config to w for all machines in the inventory.
// When the inventory has a Bastion, workers are accessed via ProxyJump through the
// bastion using their private IPs. Otherwise, workers are accessed directly via
// their public IP and NAT port.
func WriteInventoryAsSSH(w io.Writer, inventory *azuredev.Inventory, siteName, sshUser, privateKeyPath string) error {
	if inventory.Bastion != nil {
		return writeSSHWithBastion(w, inventory, siteName, sshUser, privateKeyPath)
	}

	return writeSSHDirect(w, inventory, sshUser, privateKeyPath)
}

func writeSSHWithBastion(w io.Writer, inventory *azuredev.Inventory, siteName, sshUser, privateKeyPath string) error {
	bastion := inventory.Bastion
	bastionAlias := fmt.Sprintf("bastion-%s", siteName)

	// Write the bastion host entry.
	if _, err := fmt.Fprintf(w, "Host %s\n", bastionAlias); err != nil {
		return err
	}

	if err := writeSSHHostFields(w, bastion.IPAddress, bastion.Port, sshUser, privateKeyPath, ""); err != nil {
		return err
	}

	// Write worker entries that ProxyJump through the bastion.
	for _, m := range inventory.Machines {
		if _, err := fmt.Fprintf(w, "\nHost %s\n", m.Name); err != nil {
			return err
		}

		if err := writeSSHHostFields(w, m.IPAddress, m.Port, sshUser, privateKeyPath, bastionAlias); err != nil {
			return err
		}
	}

	return nil
}

func writeSSHDirect(w io.Writer, inventory *azuredev.Inventory, sshUser, privateKeyPath string) error {
	for i, m := range inventory.Machines {
		if i > 0 {
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintf(w, "Host %s\n", m.Name); err != nil {
			return err
		}

		if err := writeSSHHostFields(w, m.IPAddress, m.Port, sshUser, privateKeyPath, ""); err != nil {
			return err
		}
	}

	return nil
}

func writeSSHHostFields(w io.Writer, hostName string, port int, user, identityFile, proxyJump string) error {
	if _, err := fmt.Fprintf(w, "    HostName %s\n", hostName); err != nil {
		return err
	}

	if port != 0 && port != 22 {
		if _, err := fmt.Fprintf(w, "    Port %d\n", port); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(w, "    User %s\n", user); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "    IdentityFile %s\n", identityFile); err != nil {
		return err
	}

	if proxyJump != "" {
		if _, err := fmt.Fprintf(w, "    ProxyJump %s\n", proxyJump); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "    StrictHostKeyChecking no\n"); err != nil {
		return err
	}

	if _, err := fmt.Fprint(w, "    UserKnownHostsFile /dev/null\n"); err != nil {
		return err
	}

	return nil
}
