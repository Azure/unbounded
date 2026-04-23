// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventory

import (
	"context"
	"fmt"

	inventoryv1 "github.com/Azure/unbounded/api/inventory/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RemoteWriter dials the inventory-collector gRPC service at addr and submits the
// full set of collected inventory data. addr should be a host:port string.
func (inv *Inventory) RemoteWriter(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connecting to inventory-collector at %s: %w", addr, err)
	}
	defer conn.Close() //nolint:errcheck

	client := inventoryv1.NewInventoryAggregatorClient(conn)

	req := &inventoryv1.SubmitInventoryRequest{
		Devices:   toProtoDevices(inv.DeviceRecords),
		Neighbors: toProtoNeighbors(inv.NeighborRecords),
	}

	resp, err := client.SubmitInventory(ctx, req)
	if err != nil {
		return fmt.Errorf("submitting inventory: %w", err)
	}

	fmt.Printf("Inventory published: %d devices saved, %d neighbors saved\n",
		resp.GetDevicesSaved(), resp.GetNeighborsSaved())

	return nil
}

func toProtoDevices(records []DeviceRecord) []*inventoryv1.DeviceRecord {
	out := make([]*inventoryv1.DeviceRecord, len(records))
	for i, r := range records {
		out[i] = &inventoryv1.DeviceRecord{
			DeviceType:     string(r.DeviceType),
			DeviceName:     r.DeviceName,
			HostIdentifier: r.HostIdentifier,
			SerialNumber:   r.SerialNumber,
			Attributes:     r.Attributes,
		}
	}

	return out
}

func toProtoNeighbors(records []NeighborRecord) []*inventoryv1.NeighborRecord {
	out := make([]*inventoryv1.NeighborRecord, len(records))
	for i, r := range records {
		out[i] = &inventoryv1.NeighborRecord{
			HostIdentifier: r.HostIdentifier,
			LocalInterface: r.LocalInterface,
			Attributes:     r.Attributes,
		}
	}

	return out
}
