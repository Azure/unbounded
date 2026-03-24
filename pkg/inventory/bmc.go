package inventory

import (
	"context"
	"fmt"

	ipmi "github.com/bougou/go-ipmi"
)

// BMCAttributes contains BMC network fields stored in DeviceRecord.Attributes.
type BMCAttributes struct {
	IPAddr     string `json:"ip_address,omitempty"`
	SubnetMask string `json:"subnet_mask,omitempty"`
	MACAddr    string `json:"mac_address,omitempty"`
	Gateway    string `json:"gateway,omitempty"`
	IPSource   string `json:"ip_source,omitempty"`
	Firmware   string `json:"firmware,omitempty"`
}

// BMCInfo holds information about an out-of-band management controller.
type BMCInfo struct {
	MACAddr    string
	IPAddr     string
	SubnetMask string
	Gateway    string
	IPSource   string
	Firmware   string
}

// BMCToRecord converts a BMCInfo into a DeviceRecord.
func BMCToRecord(b *BMCInfo, hostID string) DeviceRecord {
	serial := b.MACAddr
	if serial == "" {
		serial = "bmc-0"
	}
	return DeviceRecord{
		DeviceType:     DeviceTypeBMC,
		DeviceName:     "BMC",
		HostIdentifier: hostID,
		SerialNumber:   serial,
		Attributes: mustMarshal(BMCAttributes{
			IPAddr:     b.IPAddr,
			SubnetMask: b.SubnetMask,
			MACAddr:    b.MACAddr,
			Gateway:    b.Gateway,
			IPSource:   b.IPSource,
			Firmware:   b.Firmware,
		}),
	}
}

// collectBMCInfo attempts to discover a baseboard management controller via the
// local /dev/ipmi0 device. Returns nil (no error) when no BMC is present.
func collectBMCInfo() (*BMCInfo, error) {
	client, err := ipmi.NewOpenClient()
	if err != nil {
		return nil, nil // no IPMI device available
	}

	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		return nil, nil // cannot connect to BMC
	}
	defer client.Close(ctx)

	info := &BMCInfo{}

	// Get LAN configuration (channel 1 is the default BMC LAN channel).
	if lanCfg, err := client.GetLanConfig(ctx, 1); err == nil {
		info.IPAddr = lanCfg.IP.String()
		info.SubnetMask = lanCfg.SubnetMask.String()
		info.MACAddr = lanCfg.MAC.String()
		info.Gateway = lanCfg.DefaultGatewayIP.String()
		info.IPSource = lanCfg.IPSource.String()
	}

	// Get firmware revision from device ID.
	if devID, err := client.GetDeviceID(ctx); err == nil {
		info.Firmware = fmt.Sprintf("%d.%02d", devID.MajorFirmwareRevision, devID.MinorFirmwareRevision)
	}

	if info.IPAddr == "" && info.MACAddr == "" {
		return nil, nil
	}

	return info, nil
}

// printBMCInfo prints the collected BMC information to stdout.
func printBMCInfo(b *BMCInfo) {
	fmt.Println("BMC (Out-of-Band Management):")
	if b.IPAddr != "" {
		fmt.Printf("  IP Address:    %s\n", b.IPAddr)
	}
	if b.SubnetMask != "" {
		fmt.Printf("  Subnet Mask:   %s\n", b.SubnetMask)
	}
	if b.MACAddr != "" {
		fmt.Printf("  MAC Address:   %s\n", b.MACAddr)
	}
	if b.Gateway != "" {
		fmt.Printf("  Gateway:       %s\n", b.Gateway)
	}
	if b.IPSource != "" {
		fmt.Printf("  IP Source:     %s\n", b.IPSource)
	}
	if b.Firmware != "" {
		fmt.Printf("  Firmware:      %s\n", b.Firmware)
	}
}
