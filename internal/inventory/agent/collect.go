// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventory

import (
	"context"
	"fmt"
	"os"
	"time"
)

type ExecuteInventoryConfig struct {
	Debug         bool
	DbPath        string
	CollectorAddr string
}

// CollectInventory gathers inventory data from the environment.
// dbPath specifies where the output database file will be written.
func CollectInventory(ctx context.Context, debug bool) (*Inventory, error) {
	fmt.Println("Collecting node inventory data...")

	records := Inventory{}

	chassisRecords, hostID, err := collectChassisInfo(ctx, debug)
	if err != nil {
		fmt.Printf("Warning: %v\n", err)
	}

	records.DeviceRecords = append(records.DeviceRecords, chassisRecords...)

	bmcRecords, err := collectBMCInfo(ctx, hostID, debug)
	if err != nil {
		fmt.Printf("Error collecting BMC info: %v\n", err)
	}

	records.DeviceRecords = append(records.DeviceRecords, bmcRecords...)

	cpuRecords, err := collectCpuInfo(ctx, hostID, debug, cpuInfoPath)
	if err != nil {
		fmt.Printf("Error collecting CPU info: %v\n", err)
	}

	records.DeviceRecords = append(records.DeviceRecords, cpuRecords...)

	memRecords, err := collectMemoryInfo(ctx, hostID, debug, memInfoPath)
	if err != nil {
		fmt.Printf("Error collecting memory info: %v\n", err)
	}

	records.DeviceRecords = append(records.DeviceRecords, memRecords...)

	diskRecords, err := collectDiskInfo(ctx, hostID, debug)
	if err != nil {
		fmt.Printf("Error collecting disk info: %v\n", err)
	}

	records.DeviceRecords = append(records.DeviceRecords, diskRecords...)

	nicRecords, err := collectNICInfo(ctx, hostID, debug)
	if err != nil {
		fmt.Printf("Error collecting NIC info: %v\n", err)
	}

	records.DeviceRecords = append(records.DeviceRecords, nicRecords...)

	gpuRecords, err := collectGPUInfo(ctx, hostID, debug)
	if err != nil {
		fmt.Printf("Error collecting GPU info: %v\n", err)
	}

	records.DeviceRecords = append(records.DeviceRecords, gpuRecords...)

	fmt.Printf("Done collecting devices. Found %d devices total\n", len(records.DeviceRecords))

	// Collect LLDP neighbor information.
	networkNeighbors, err := collectLLDPNeighbors(ctx, hostID)
	if err != nil {
		fmt.Printf("Warning: LLDP neighbor discovery failed: %v\n", err)
	} else if debug {
		printLLDPNeighbors(networkNeighbors)
	}

	fmt.Printf("Found %d LLDP neighbors\n", len(networkNeighbors))

	records.NeighborRecords = append(records.NeighborRecords, networkNeighbors...)

	// Collect IMEX domain peers (NVLink/NVSwitch fabric).
	imexNeighbors, err := collectIMEXNeighbors(ctx, hostID)
	if err != nil {
		fmt.Printf("Warning: IMEX neighbor discovery failed: %v\n", err)
	} else if debug {
		printIMEXNeighbors(imexNeighbors)
	}

	fmt.Printf("Found %d NVLink IMEX neighbors\n", len(imexNeighbors))

	records.NeighborRecords = append(records.NeighborRecords, imexNeighbors...)

	return &records, nil
}

func Execute(config ExecuteInventoryConfig) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("error: inventory must be run as root")
	}

	if config.Debug {
		fmt.Println("Running in debug mode")
	}

	fmt.Printf("Output database: %s\n", config.DbPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := ensureDB(config.DbPath); err != nil {
		return fmt.Errorf("error initializing database: %w", err)
	}

	inventory, err := CollectInventory(ctx, config.Debug)
	if err != nil {
		return err
	}

	if err := inventory.localDWriter(ctx, config.DbPath); err != nil {
		return fmt.Errorf("error writing inventory to database: %w", err)
	}

	if err := inventory.RemoteWriter(ctx, config.CollectorAddr); err != nil {
		return fmt.Errorf("error publishing inventory: %w", err)
	}

	return nil
}
