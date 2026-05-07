// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventoryviewer

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/lib/pq"
)

//go:embed static
var staticFiles embed.FS

// Config holds the configuration for the inventory viewer.
type Config struct {
	Addr   string
	DbConn pq.Config
}

// DeviceRow represents a row from the inventory table.
type DeviceRow struct {
	ID             int64  `json:"id"`
	DeviceType     string `json:"device_type"`
	DeviceName     string `json:"device_name"`
	HostIdentifier string `json:"host_identifier"`
	SerialNumber   string `json:"serial_number"`
	Attributes     string `json:"attributes"`
}

// NeighborRow represents a row from the neighbors table.
type NeighborRow struct {
	ID             int64  `json:"id"`
	HostIdentifier string `json:"host_identifier"`
	LocalInterface string `json:"local_interface"`
	Attributes     string `json:"attributes"`
}

// Execute starts the inventory viewer web server and blocks until the context
// is cancelled.
func Execute(ctx context.Context, cfg Config) error {
	connector, err := pq.NewConnectorConfig(cfg.DbConn)
	if err != nil {
		return fmt.Errorf("creating database connector: %w", err)
	}

	db := sql.OpenDB(connector)
	defer db.Close() //nolint:errcheck

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}

	slog.Info("database connection successful",
		"db_host", cfg.DbConn.Host,
		"db_port", cfg.DbConn.Port,
		"db_name", cfg.DbConn.Database,
	)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		handleDevices(w, r, db)
	})

	mux.HandleFunc("GET /api/neighbors", func(w http.ResponseWriter, r *http.Request) {
		handleNeighbors(w, r, db)
	})

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("creating static sub-filesystem: %w", err)
	}

	mux.Handle("GET /", http.FileServer(http.FS(staticFS)))

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down HTTP server")
		srv.Shutdown(context.Background()) //nolint:errcheck
	}()

	slog.Info("HTTP server listening", "addr", cfg.Addr)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving HTTP: %w", err)
	}

	return nil
}

func handleDevices(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	rows, err := db.QueryContext(r.Context(),
		`SELECT id, device_type, device_name, host_identifier, serial_number, attributes
		 FROM inventory ORDER BY id`)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		slog.Error("querying devices", "error", err)

		return
	}
	defer rows.Close() //nolint:errcheck

	devices := []DeviceRow{}

	for rows.Next() {
		var d DeviceRow
		if err := rows.Scan(&d.ID, &d.DeviceType, &d.DeviceName, &d.HostIdentifier, &d.SerialNumber, &d.Attributes); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			slog.Error("scanning device row", "error", err)

			return
		}

		devices = append(devices, d)
	}

	if err := rows.Err(); err != nil {
		http.Error(w, "iteration failed", http.StatusInternalServerError)
		slog.Error("iterating device rows", "error", err)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(devices) //nolint:errcheck
}

func handleNeighbors(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	rows, err := db.QueryContext(r.Context(),
		`SELECT id, host_identifier, local_interface, attributes
		 FROM neighbors ORDER BY id`)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		slog.Error("querying neighbors", "error", err)

		return
	}
	defer rows.Close() //nolint:errcheck

	neighbors := []NeighborRow{}

	for rows.Next() {
		var n NeighborRow
		if err := rows.Scan(&n.ID, &n.HostIdentifier, &n.LocalInterface, &n.Attributes); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			slog.Error("scanning neighbor row", "error", err)

			return
		}

		neighbors = append(neighbors, n)
	}

	if err := rows.Err(); err != nil {
		http.Error(w, "iteration failed", http.StatusInternalServerError)
		slog.Error("iterating neighbor rows", "error", err)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(neighbors) //nolint:errcheck
}
