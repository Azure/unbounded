package commands

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const poolLabel = "unbounded-kube.io/pool"

// ANSI color/style codes for terminal output.
const (
	bold  = "\033[1m"
	dim   = "\033[2m"
	reset = "\033[0m"
	green = "\033[32m"
	cyan  = "\033[36m"
)

// StatusUpdater implements attestation.StatusUpdater using a controller-runtime client.
type StatusUpdater struct {
	Client client.Client
}

func (u *StatusUpdater) Update(ctx context.Context, node *v1alpha3.Machine) error {
	return u.Client.Status().Update(ctx, node)
}

func BuildScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

func PoolSelector(pool string) (labels.Selector, error) {
	var (
		op   selection.Operator
		vals []string
	)

	if pool != "" {
		op = selection.Equals
		vals = []string{pool}
	} else {
		op = selection.DoesNotExist
	}

	req, err := labels.NewRequirement(poolLabel, op, vals)
	if err != nil {
		return nil, err
	}

	return labels.NewSelector().Add(*req), nil
}

func DefaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".unbounded", "metalman", "cache")
	}

	return filepath.Join(home, ".unbounded", "metalman", "cache")
}

func LeaderElectionID(pool string) string {
	if pool == "" {
		return "metalman"
	}

	return "metalman-" + pool
}

func PrintStep(msg string) {
	fmt.Printf("  %s-->%s %s\n", cyan, reset, msg)
}

func PrintConfig(key, value string) {
	fmt.Printf("  %s%-18s%s %s\n", dim, key, reset, value)
}

func PrintReady() {
	fmt.Printf("\n  %s%sready%s\n\n", green, bold, reset)
}

func PrintService(protocol, address string) {
	fmt.Printf("  %s%-8s%s %s\n", bold, protocol, reset, address)
}

func OutboundIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // Best-effort close of UDP probe connection.

	return conn.LocalAddr().(*net.UDPAddr).IP, nil //nolint:errcheck // Type is guaranteed by net.Dial("udp", ...).
}

func InterfaceIPv4(name string) (net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}

		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4, nil
		}
	}

	return nil, fmt.Errorf("no IPv4 address on interface %s", name)
}
