package inventory

import (
	"context"
	"fmt"
	"os"
	"time"
)

// CollectInventory gathers inventory data from the environment.
// dbPath specifies where the output database file will be written.
func CollectInventory(debug bool, dbPath string) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "error: inventory must be run as root")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("Collecting node inventory data...")
	fmt.Printf("Output database: %s\n", dbPath)

	if debug {
		fmt.Println("Running in debug mode")
	}

	if err := ensureDB(dbPath); err != nil {
		fmt.Printf("Error initializing database: %v\n", err)
		return
	}

	chassisInfo, err := collectChassisInfo(ctx)
	if err != nil {
		fmt.Printf("Warning: %v\n", err)
	}

	if debug && chassisInfo != nil {
		printChassisInfo(chassisInfo)
	}

	bmcInfo, err := collectBMCInfo(ctx)
	if err != nil {
		fmt.Printf("Error collecting BMC info: %v\n", err)
	} else if bmcInfo != nil && debug {
		printBMCInfo(bmcInfo)
	}

	cpuInfo, err := collectCpuInfo(ctx)
	if err != nil {
		fmt.Printf("Error collecting CPU info: %v\n", err)
	} else if debug {
		printCpuInfo(cpuInfo)
	}

	memInfo, err := collectMemoryInfo(ctx)
	if err != nil {
		fmt.Printf("Error collecting memory info: %v\n", err)
	} else if debug {
		printMemoryInfo(memInfo)
	}

	diskInfo, err := collectDiskInfo(ctx)
	if err != nil {
		fmt.Printf("Error collecting disk info: %v\n", err)
	} else if debug {
		printDiskInfo(diskInfo)
	}

	nicInfo, err := collectNICInfo(ctx)
	if err != nil {
		fmt.Printf("Error collecting NIC info: %v\n", err)
	} else if debug {
		printNICInfo(nicInfo)
	}

	gpuInfo, err := collectGPUInfo(ctx)
	if err != nil {
		fmt.Printf("Error collecting GPU info: %v\n", err)
	} else if debug {
		printGPUInfo(gpuInfo)
	}

	// Derive the host identifier from the chassis serial (which already
	// includes the DMI serial → product_uuid → machine-id fallback chain).
	var hostID string
	if chassisInfo != nil {
		hostID = chassisInfo.SerialNumber
	}

	// Build DeviceRecords from all collected components.
	var records []DeviceRecord

	records = append(records, ChassisToRecord(chassisInfo, hostID))

	if bmcInfo != nil {
		records = append(records, BMCToRecord(bmcInfo, hostID))
	}

	if cpuInfo != nil {
		records = append(records, CPUToRecord(cpuInfo, hostID))
	}

	if memInfo != nil {
		records = append(records, MemoryToRecord(memInfo, hostID))
	}

	for i := range diskInfo {
		records = append(records, DiskToRecord(&diskInfo[i], hostID))
	}

	for i := range nicInfo {
		records = append(records, NICToRecord(&nicInfo[i], hostID))
	}

	for i := range gpuInfo {
		records = append(records, GPUToRecord(&gpuInfo[i], hostID))
	}

	if err := upsertRecords(dbPath, records); err != nil {
		fmt.Printf("Error writing to database: %v\n", err)
		return
	}

	fmt.Printf("Done collecting. Wrote %d device records to %s\n", len(records), dbPath)

	// Collect LLDP neighbor information.
	neighbors, err := collectLLDPNeighbors(ctx, hostID)
	if err != nil {
		fmt.Printf("Warning: LLDP neighbor discovery failed: %v\n", err)
	} else {
		if debug {
			printLLDPNeighbors(neighbors)
		}

		if len(neighbors) > 0 {
			if err := upsertNeighbors(dbPath, neighbors); err != nil {
				fmt.Printf("Error writing neighbors to database: %v\n", err)
				return
			}

			fmt.Printf("Wrote %d LLDP neighbor records to %s\n", len(neighbors), dbPath)
		}
	}

	// Collect IMEX domain peers (NVLink/NVSwitch fabric).
	imexNeighbors, err := collectIMEXNeighbors(ctx, hostID)
	if err != nil {
		fmt.Printf("Warning: IMEX neighbor discovery failed: %v\n", err)
	} else if len(imexNeighbors) > 0 {
		if debug {
			printIMEXNeighbors(imexNeighbors)
		}

		if err := upsertNeighbors(dbPath, imexNeighbors); err != nil {
			fmt.Printf("Error writing IMEX neighbors to database: %v\n", err)
			return
		}

		fmt.Printf("Wrote %d IMEX neighbor records to %s\n", len(imexNeighbors), dbPath)
	}
}
