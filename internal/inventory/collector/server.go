// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventorycollector

import (
	"context"
	"database/sql"
	"log/slog"

	inventoryv1 "github.com/Azure/unbounded-kube/api/inventory/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements the InventoryCollector gRPC service.
type Server struct {
	inventoryv1.UnimplementedInventoryCollectorServer
	db *sql.DB
}

// NewServer returns a new gRPC server backed by the given database connection.
func NewServer(db *sql.DB) *Server {
	return &Server{db: db}
}

// SubmitInventory receives a batch of device and neighbor records and persists
// them to the PostgreSQL database.
func (s *Server) SubmitInventory(ctx context.Context, req *inventoryv1.SubmitInventoryRequest) (*inventoryv1.SubmitInventoryResponse, error) {
	devicesSaved, err := UpsertDevices(ctx, s.db, req.GetDevices())
	if err != nil {
		slog.Error("failed to upsert devices", "error", err)

		return nil, status.Errorf(codes.Internal, "upserting devices: %v", err)
	}

	neighborsSaved, err := UpsertNeighbors(ctx, s.db, req.GetNeighbors())
	if err != nil {
		slog.Error("failed to upsert neighbors", "error", err)

		return nil, status.Errorf(codes.Internal, "upserting neighbors: %v", err)
	}

	slog.Info("inventory submitted", "devices_saved", devicesSaved, "neighbors_saved", neighborsSaved)

	return &inventoryv1.SubmitInventoryResponse{
		DevicesSaved:   int32(devicesSaved),
		NeighborsSaved: int32(neighborsSaved),
	}, nil
}
