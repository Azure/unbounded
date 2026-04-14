// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventorycollector

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/lib/pq"
	"google.golang.org/grpc"

	inventoryv1 "github.com/Azure/unbounded-kube/api/inventory/v1"
)

// Config holds the configuration for the inventory collector service.
type Config struct {
	Debug    bool
	DbConn   pq.Config
	GRPCAddr string
}

// Run starts the inventory collector service.
func Run(ctx context.Context, cfg Config) error {
	slog.Info("starting inventory-collector")

	db, err := OpenDatabase(cfg.DbConn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}

	slog.Info("database connection successful", "db_host", cfg.DbConn.Host, "db_port", cfg.DbConn.Port, "db_name", cfg.DbConn.Database, "db_user", cfg.DbConn.User)

	defer db.Close() //nolint:errcheck

	if err := EnsureSchema(db, cfg.DbConn.Database); err != nil {
		return fmt.Errorf("ensuring database schema: %w", err)
	}

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("error listening on %s: %w", cfg.GRPCAddr, err)
	}

	grpcServer := grpc.NewServer()
	inventoryv1.RegisterInventoryCollectorServer(grpcServer, NewServer(db))

	// Shut down gracefully when the context is cancelled.
	go func() {
		<-ctx.Done()
		slog.Info("shutting down gRPC server")
		grpcServer.GracefulStop()
	}()

	slog.Info("gRPC server listening", "addr", cfg.GRPCAddr)

	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("serving gRPC: %w", err)
	}

	slog.Info("inventory-collector stopped")

	return nil
}
