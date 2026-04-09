package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/internal/provision"
)

type configureGatewayRoutes struct {
	log     *slog.Logger
	network *provision.AgentNetworkConfig
}

// ConfigureGatewayRoutes returns a task that adds /32 host routes for each
// address in the Network.GatewayRoutes list. This makes the addresses
// appear directly connected so that CNI plugins (e.g. kindnet) can use
// them as nexthops when adding pod-CIDR routes across subnets.
//
// When network is nil or has no gateway routes the task is a no-op.
func ConfigureGatewayRoutes(log *slog.Logger, network *provision.AgentNetworkConfig) phases.Task {
	return &configureGatewayRoutes{log: log, network: network}
}

func (c *configureGatewayRoutes) Name() string { return "configure-gateway-routes" }

func (c *configureGatewayRoutes) Do(ctx context.Context) error {
	if c.network == nil || len(c.network.GatewayRoutes) == 0 {
		return nil
	}

	iface, err := defaultRouteInterface()
	if err != nil {
		return fmt.Errorf("detecting default route interface: %w", err)
	}

	c.log.Info("configuring gateway host-routes",
		"interface", iface,
		"routes", c.network.GatewayRoutes,
	)

	for _, gw := range c.network.GatewayRoutes {
		if net.ParseIP(gw) == nil {
			return fmt.Errorf("invalid gateway IP: %s", gw)
		}

		// ip route replace <gw>/32 dev <iface>
		// "replace" is idempotent — safe to re-run.
		cmd := exec.CommandContext(ctx, "ip", "route", "replace", gw+"/32", "dev", iface)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("adding gateway route %s/32 dev %s: %w (%s)", gw, iface, err, string(out))
		}

		c.log.Info("added gateway host-route", "gateway", gw, "interface", iface)
	}

	return nil
}

// defaultRouteInterface returns the name of the network interface used by
// the default route (0.0.0.0/0 or ::/0). It shells out to "ip route" for
// simplicity rather than using netlink directly.
func defaultRouteInterface() (string, error) {
	out, err := exec.Command("ip", "-o", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("ip route show default: %w", err)
	}

	// Output format: "default via <gw> dev <iface> ..."
	fields := splitFields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}

	return "", fmt.Errorf("no default route found")
}

// splitFields splits a string on whitespace, ignoring empty fields.
func splitFields(s string) []string {
	var fields []string

	start := -1

	for i, ch := range s {
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			if start >= 0 {
				fields = append(fields, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}

	if start >= 0 {
		fields = append(fields, s[start:])
	}

	return fields
}
