package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// LLDPNeighborAttributes holds the parsed LLDP TLV fields for a single
// neighbor discovered on a local interface.
type LLDPNeighborAttributes struct {
	ChassisID         string `json:"chassisId"`
	PortID            string `json:"portId"`
	PortDescription   string `json:"portDesc"`
	SystemName        string `json:"systemName"`
	SystemDescription string `json:"systemDesc"`
	MgmtAddresses     string `json:"mgmtAddresses"`
}

// collectLLDPNeighbors runs `lldpctl -f json` and parses the output into
// NeighborRecords ready for insertion into the neighbors table.
func collectLLDPNeighbors(ctx context.Context, hostID string) ([]NeighborRecord, error) {
	out, err := exec.CommandContext(ctx, "lldpctl", "-f", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("lldpctl: %w", err)
	}

	return parseLLDPNeighbors(hostID, out)
}

// parseLLDPNeighbors parses raw `lldpctl -f json` output into NeighborRecords.
// Separated from collectLLDPNeighbors for testability.
func parseLLDPNeighbors(hostID string, data []byte) ([]NeighborRecord, error) {
	// lldpctl JSON can vary: the "interface" key may be a single object or
	// an array. We first try to unmarshal into the canonical array form,
	// then fall back to the single-object form.
	var records []NeighborRecord

	// Raw intermediate parse so we can handle both shapes.
	var raw struct {
		Lldp json.RawMessage `json:"lldp"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("lldpctl: failed to parse output: %w", err)
	}

	interfaces, err := parseLLDPInterfaces(raw.Lldp)
	if err != nil {
		return nil, err
	}

	for localIface, neighbors := range interfaces {
		for _, n := range neighbors {
			attrs := LLDPNeighborAttributes(n)
			records = append(records, NeighborRecord{
				HostIdentifier: hostID,
				LocalInterface: localIface,
				Attributes:     mustMarshal(attrs),
			})
		}
	}

	return records, nil
}

// parsedNeighbor is an intermediate representation used during JSON parsing.
type parsedNeighbor struct {
	ChassisID         string
	PortID            string
	PortDescription   string
	SystemName        string
	SystemDescription string
	MgmtAddresses     string
}

// parseLLDPInterfaces extracts per-interface neighbor lists from the raw "lldp"
// JSON value. It handles the polymorphic interface field that lldpctl emits.
func parseLLDPInterfaces(lldpRaw json.RawMessage) (map[string][]parsedNeighbor, error) {
	result := make(map[string][]parsedNeighbor)

	// The "lldp" object contains an "interface" key whose value is either
	// a JSON object (single interface) or array (multiple interfaces).
	// Each interface object is keyed by interface name.
	var wrapper struct {
		Interface json.RawMessage `json:"interface"`
	}
	if err := json.Unmarshal(lldpRaw, &wrapper); err != nil {
		return nil, fmt.Errorf("lldpctl: failed to parse lldp block: %w", err)
	}

	if wrapper.Interface == nil {
		return result, nil
	}

	// Try array of objects first.
	var ifaceArray []json.RawMessage
	if err := json.Unmarshal(wrapper.Interface, &ifaceArray); err == nil {
		for _, raw := range ifaceArray {
			parseInterfaceObject(raw, result)
		}

		return result, nil
	}

	// Fall back to single object.
	parseInterfaceObject(wrapper.Interface, result)

	return result, nil
}

// parseInterfaceObject parses a single JSON object whose keys are interface
// names and values contain neighbor/chassis data.
func parseInterfaceObject(raw json.RawMessage, out map[string][]parsedNeighbor) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}

	for ifaceName, val := range obj {
		n := extractNeighbor(val)
		out[ifaceName] = append(out[ifaceName], n)
	}
}

// extractNeighbor pulls LLDP TLV fields from a neighbor JSON blob.
func extractNeighbor(data json.RawMessage) parsedNeighbor {
	var n parsedNeighbor

	var entry struct {
		Chassis json.RawMessage `json:"chassis"`
		Port    json.RawMessage `json:"port"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return n
	}

	// Chassis is an object keyed by the system name.
	if entry.Chassis != nil {
		var chassisMap map[string]json.RawMessage
		if err := json.Unmarshal(entry.Chassis, &chassisMap); err == nil {
			for chassisName, cData := range chassisMap {
				n.SystemName = chassisName

				var c struct {
					ID     json.RawMessage `json:"id"`
					Descr  json.RawMessage `json:"descr"`
					MgmtIP json.RawMessage `json:"mgmt-ip"`
				}
				if err := json.Unmarshal(cData, &c); err == nil {
					n.ChassisID = extractIDValue(c.ID)
					n.SystemDescription = extractStringOrValue(c.Descr)
					n.MgmtAddresses = parseMgmtAddresses(c.MgmtIP)
				}

				break // take only the first chassis entry
			}
		}
	}

	// Port fields.
	if entry.Port != nil {
		var p struct {
			ID    json.RawMessage `json:"id"`
			Descr json.RawMessage `json:"descr"`
		}
		if err := json.Unmarshal(entry.Port, &p); err == nil {
			n.PortID = extractIDValue(p.ID)
			n.PortDescription = extractStringOrValue(p.Descr)
		}
	}

	return n
}

// extractIDValue parses an "id" field that may be a single object
// {"type":"...","value":"..."} or an array of such objects.
func extractIDValue(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	// Try single object first (most common lldpctl output).
	var single struct{ Value string }
	if err := json.Unmarshal(raw, &single); err == nil && single.Value != "" {
		return single.Value
	}
	// Try array of objects.
	var arr []struct{ Value string }
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0].Value
	}

	return ""
}

// extractStringOrValue parses a field that may be a plain JSON string, a
// single {"value":"..."} object, or an array of such objects.
func extractStringOrValue(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	// Try plain string (lldpctl uses this for "descr").
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try single object.
	var single struct{ Value string }
	if err := json.Unmarshal(raw, &single); err == nil && single.Value != "" {
		return single.Value
	}
	// Try array of objects.
	var arr []struct{ Value string }
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0].Value
	}

	return ""
}

// parseMgmtAddresses handles the mgmt-ip field which can be a single string
// object or an array of string objects.
func parseMgmtAddresses(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}

	// Try array of objects.
	var arr []struct{ Value string }
	if err := json.Unmarshal(raw, &arr); err == nil {
		addrs := make([]string, 0, len(arr))
		for _, a := range arr {
			if a.Value != "" {
				addrs = append(addrs, a.Value)
			}
		}

		return strings.Join(addrs, ",")
	}

	// Try single string.
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single
	}

	return ""
}

// printLLDPNeighbors prints discovered LLDP neighbors to stdout.
func printLLDPNeighbors(neighbors []NeighborRecord) {
	fmt.Printf("LLDP Neighbors Found: %d\n", len(neighbors))

	for i, n := range neighbors {
		var attrs LLDPNeighborAttributes

		if err := json.Unmarshal(n.Attributes, &attrs); err != nil {
			fmt.Printf("\n  Neighbor %d (%s): <failed to parse attributes: %v>\n", i, n.LocalInterface, err)

			continue
		}

		fmt.Printf("\n  Neighbor %d (%s):\n", i, n.LocalInterface)

		if attrs.ChassisID != "" {
			fmt.Printf("    Chassis ID:  %s\n", attrs.ChassisID)
		}

		if attrs.PortID != "" {
			fmt.Printf("    Port ID:     %s\n", attrs.PortID)
		}

		if attrs.PortDescription != "" {
			fmt.Printf("    Port Description:   %s\n", attrs.PortDescription)
		}

		if attrs.SystemName != "" {
			fmt.Printf("    System Name: %s\n", attrs.SystemName)
		}

		if attrs.SystemDescription != "" {
			fmt.Printf("    System Description: %s\n", attrs.SystemDescription)
		}

		if attrs.MgmtAddresses != "" {
			fmt.Printf("    Mgmt Addrs:  %s\n", attrs.MgmtAddresses)
		}
	}
}
