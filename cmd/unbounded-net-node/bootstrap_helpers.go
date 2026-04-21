// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded-kube/internal/version"
)

// errCNIConfDirNotEmpty is returned when existing CNI configuration files are
// found in the CNI conf directory, indicating another CNI plugin is installed.
var errCNIConfDirNotEmpty = errors.New("CNI configuration directory is not empty")

// checkCNIConfDirForConflists checks whether the CNI configuration directory
// already contains any *.conflist files other than the node agent's own file.
// Returns errCNIConfDirNotEmpty if foreign conflist files are found, which
// indicates another CNI plugin is installed.
func checkCNIConfDirForConflists(dir, ownConfFile string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "*.conflist"))
	if err != nil {
		return fmt.Errorf("failed to glob CNI conf directory %s: %w", dir, err)
	}

	names := make([]string, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		if base == ownConfFile {
			continue
		}

		names = append(names, base)
	}

	if len(names) > 0 {
		return fmt.Errorf(
			"%w: found existing conflist files in %s: %s -- "+
				"unbounded-net refuses to overwrite an existing CNI configuration",
			errCNIConfDirNotEmpty, dir, strings.Join(names, ", "),
		)
	}

	return nil
}

func waitForPodCIDRsAndConfigure(ctx context.Context, clientset kubernetes.Interface, cfg *config, cniConfigured *bool) ([]string, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Get the node to check if podCIDRs are already set
		node, err := clientset.CoreV1().Nodes().Get(ctx, cfg.NodeName, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Failed to get node %s: %v", cfg.NodeName, err)
			time.Sleep(5 * time.Second)

			continue
		}

		// Check if podCIDRs are already assigned
		if len(node.Spec.PodCIDRs) > 0 {
			klog.Infof("Node %s has podCIDRs: %v", cfg.NodeName, node.Spec.PodCIDRs)

			if err := writeCNIConfig(cfg, node.Spec.PodCIDRs); err != nil {
				if errors.Is(err, errCNIConfDirNotEmpty) {
					return nil, err
				}

				klog.Errorf("Failed to write CNI config: %v", err)
				time.Sleep(5 * time.Second)

				continue
			}

			*cniConfigured = true

			klog.Info("CNI configuration written successfully")

			return node.Spec.PodCIDRs, nil
		}

		klog.Infof("Node %s has no podCIDRs yet, watching for assignment...", cfg.NodeName)

		// Watch for podCIDR assignment
		watcher, err := clientset.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{
			FieldSelector:   fields.OneTermEqualSelector("metadata.name", cfg.NodeName).String(),
			ResourceVersion: node.ResourceVersion,
		})
		if err != nil {
			klog.Errorf("Failed to watch node: %v", err)
			time.Sleep(5 * time.Second)

			continue
		}

		// Process watch events until podCIDRs are set
		watchCh := watcher.ResultChan()

	watchLoop:
		for {
			select {
			case <-ctx.Done():
				watcher.Stop()
				return nil, ctx.Err()

			case event, ok := <-watchCh:
				if !ok {
					klog.Warning("Watch channel closed, restarting watch")
					break watchLoop
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					updatedNode, ok := event.Object.(*corev1.Node)
					if !ok {
						klog.Warning("Received non-node object from watch")
						continue
					}

					if len(updatedNode.Spec.PodCIDRs) > 0 {
						klog.Infof("Node %s podCIDRs assigned: %v", cfg.NodeName, updatedNode.Spec.PodCIDRs)
						watcher.Stop()

						if err := writeCNIConfig(cfg, updatedNode.Spec.PodCIDRs); err != nil {
							if errors.Is(err, errCNIConfDirNotEmpty) {
								return nil, err
							}

							klog.Errorf("Failed to write CNI config: %v", err)
							time.Sleep(5 * time.Second)

							break watchLoop
						}

						*cniConfigured = true

						klog.Info("CNI configuration written successfully")

						return updatedNode.Spec.PodCIDRs, nil
					}

				case watch.Deleted:
					klog.Warningf("Node %s was deleted", cfg.NodeName)
					watcher.Stop()

					return nil, fmt.Errorf("node %s was deleted", cfg.NodeName)

				case watch.Error:
					klog.Errorf("Watch error: %v", event.Object)
					break watchLoop
				}
			}
		}

		watcher.Stop()
	}
}

func writeCNIConfig(cfg *config, podCIDRs []string) error {
	start := time.Now()
	err := doWriteCNIConfig(cfg, podCIDRs)

	nodeCNIConfigWriteDuration.Observe(time.Since(start).Seconds())

	if err != nil {
		nodeCNIConfigWrites.WithLabelValues("error").Inc()
	} else {
		nodeCNIConfigWrites.WithLabelValues("success").Inc()
	}

	return err
}

func doWriteCNIConfig(cfg *config, podCIDRs []string) error {
	// Build IPAM ranges from podCIDRs
	var ranges [][]IPRange
	for _, cidr := range podCIDRs {
		ranges = append(ranges, []IPRange{{Subnet: cidr}})
	}

	// Bridge MTU matches the configured WireGuard tunnel MTU (cfg.MTU is
	// always non-zero after config normalization).
	bridgeMTU := cfg.MTU

	// Build CNI conflist
	cniConfig := CNIConfig{
		CNIVersion: "0.4.0",
		Name:       "unbounded-net",
		Plugins: []PluginConf{
			{
				Type:         "bridge",
				Bridge:       cfg.BridgeName,
				IsGateway:    true,
				IsDefaultGW:  true,
				ForceAddress: true,
				IPMasq:       false,
				HairpinMode:  true,
				MTU:          bridgeMTU,
				IPAM: &IPAMConfig{
					Type:   "host-local",
					Ranges: ranges,
				},
			},
			{
				Type: "portmap",
				Capabilities: &Caps{
					PortMappings: true,
				},
			},
			{
				Type: "loopback",
			},
		},
	}

	// Marshal to JSON with indentation
	data, err := json.MarshalIndent(cniConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal CNI config: %w", err)
	}

	// Refuse to install if the directory already contains conflist files
	if err := checkCNIConfDirForConflists(cfg.CNIConfDir, cfg.CNIConfFile); err != nil {
		return err
	}

	// Ensure the CNI conf directory exists
	if err := os.MkdirAll(cfg.CNIConfDir, 0o755); err != nil {
		return fmt.Errorf("failed to create CNI conf directory: %w", err)
	}

	// Write to a temp file first, then rename for atomic write
	confPath := filepath.Join(cfg.CNIConfDir, cfg.CNIConfFile)
	tmpPath := confPath + ".tmp"

	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write temp CNI config: %w", err)
	}

	if err := os.Rename(tmpPath, confPath); err != nil {
		return fmt.Errorf("failed to rename CNI config: %w", err)
	}

	klog.Infof("Wrote CNI config to %s", confPath)

	return nil
}

// allowedWireGuardPrefixes lists directory prefixes considered safe for WireGuard key storage.
var allowedWireGuardPrefixes = []string{"/var/", "/etc/", "/run/", "/host/"}

// validateWireGuardDir checks that dir is a safe absolute path without traversal components.
func validateWireGuardDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("wireguard directory must be an absolute path, got %q", dir)
	}

	cleaned := filepath.Clean(dir)
	for _, component := range strings.Split(cleaned, string(filepath.Separator)) {
		if component == ".." {
			return fmt.Errorf("wireguard directory must not contain path traversal components, got %q", dir)
		}
	}

	for _, prefix := range allowedWireGuardPrefixes {
		if strings.HasPrefix(cleaned+"/", prefix) {
			return nil
		}
	}

	return fmt.Errorf("wireguard directory %q is not under an allowed prefix (%s)", dir, strings.Join(allowedWireGuardPrefixes, ", "))
}

func ensureWireGuardKeys(cfg *config) (string, error) {
	if err := validateWireGuardDir(cfg.WireGuardDir); err != nil {
		return "", err
	}

	privKeyPath := filepath.Join(cfg.WireGuardDir, "server.priv")
	pubKeyPath := filepath.Join(cfg.WireGuardDir, "server.pub")

	// Attempt to open private key directly to avoid TOCTOU race
	privKeyFile, err := os.Open(privKeyPath)
	if err == nil {
		_ = privKeyFile.Close() //nolint:errcheck
		// Private key exists, read the public key
		pubKeyData, err := os.ReadFile(pubKeyPath)
		if err != nil {
			return "", fmt.Errorf("private key exists but failed to read public key: %w", err)
		}

		return string(pubKeyData), nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("failed to open private key file: %w", err)
	}

	// Generate new keys
	klog.Info("Generating new WireGuard keys")

	// Generate private key (32 random bytes, then clamp for Curve25519)
	var privateKey [32]byte
	if _, err := rand.Read(privateKey[:]); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	// Clamp the private key for Curve25519
	privateKey[0] &= 248
	privateKey[31] &= 127
	privateKey[31] |= 64

	// Verify the clamped key is not all zeros (defensive check)
	var zeroKey [32]byte
	if privateKey == zeroKey {
		return "", fmt.Errorf("generated WireGuard private key is all zeros after clamping")
	}

	// Derive public key
	var publicKey [32]byte
	curve25519.ScalarBaseMult(&publicKey, &privateKey)

	// Encode keys as base64
	privKeyB64 := base64.StdEncoding.EncodeToString(privateKey[:])
	pubKeyB64 := base64.StdEncoding.EncodeToString(publicKey[:])

	// Ensure wireguard directory exists
	if err := os.MkdirAll(cfg.WireGuardDir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create wireguard directory: %w", err)
	}

	// Write private key (mode 0600 - owner read/write only)
	if err := os.WriteFile(privKeyPath, []byte(privKeyB64), 0o600); err != nil {
		return "", fmt.Errorf("failed to write private key: %w", err)
	}

	// Write public key (mode 0644 - readable by all)
	if err := os.WriteFile(pubKeyPath, []byte(pubKeyB64), 0o644); err != nil {
		return "", fmt.Errorf("failed to write public key: %w", err)
	}

	klog.Infof("WireGuard keys written to %s", cfg.WireGuardDir)

	return pubKeyB64, nil
}

// annotationPatch is used to build a safe JSON merge patch for node annotations.
type annotationPatch struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

// annotateNodeWithPubKey adds the WireGuard public key annotation to the node.
func annotateNodeWithPubKey(ctx context.Context, clientset kubernetes.Interface, nodeName, pubKey string) error {
	var p annotationPatch

	p.Metadata.Annotations = map[string]string{WireGuardPubKeyAnnotation: pubKey}

	patch, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("failed to marshal annotation patch: %w", err)
	}

	_, err = clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node: %w", err)
	}

	return nil
}

// annotateNodeWithMTU adds (or updates) the tunnel MTU annotation on the node.
func annotateNodeWithMTU(ctx context.Context, clientset kubernetes.Interface, nodeName string, mtu int) error {
	var p annotationPatch

	p.Metadata.Annotations = map[string]string{TunnelMTUAnnotation: fmt.Sprintf("%d", mtu)}

	patch, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("failed to marshal MTU annotation patch: %w", err)
	}

	_, err = clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node with MTU annotation: %w", err)
	}

	return nil
}

// taintGatewayNode adds a NoSchedule taint to gateway nodes to prevent regular workloads
// from being scheduled on them (since they don't have regular pod CIDR routing)
func taintGatewayNode(ctx context.Context, clientset kubernetes.Interface, nodeName string) error {
	// Get current node to check if taint already exists
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	// Check if taint already exists
	for _, taint := range node.Spec.Taints {
		if taint.Key == GatewayNodeTaintKey {
			klog.V(2).Infof("Gateway taint already exists on node %s", nodeName)
			return nil
		}
	}

	// Add the gateway taint using a JSON patch to append to existing taints
	// JSON Patch RFC 6902: "add" operation with "-" appends to array
	var patch []byte
	if len(node.Spec.Taints) == 0 {
		// No existing taints - create the taints array with our taint
		patch = []byte(fmt.Sprintf(`[{"op":"add","path":"/spec/taints","value":[{"key":"%s","value":"true","effect":"NoSchedule"}]}]`,
			GatewayNodeTaintKey))
	} else {
		// Existing taints - append to the array
		patch = []byte(fmt.Sprintf(`[{"op":"add","path":"/spec/taints/-","value":{"key":"%s","value":"true","effect":"NoSchedule"}}]`,
			GatewayNodeTaintKey))
	}

	_, err = clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to taint node: %w", err)
	}

	klog.Infof("Added gateway taint to node %s: %s=true:NoSchedule", nodeName, GatewayNodeTaintKey)

	return nil
}

func nodeAgentBuildInfo() *BuildInfo {
	return &BuildInfo{
		Version:   version.Version,
		Commit:    version.GitCommit,
		BuildTime: version.BuildTime,
	}
}
