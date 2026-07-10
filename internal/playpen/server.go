// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const bmcShutdownTimeout = 10 * time.Second

func ServeBMC(ctx context.Context, cfg Config, vm *VMManager) error {
	if err := ensureBMCCertificate(cfg); err != nil {
		return err
	}

	listener, err := net.Listen("tcp", cfg.BMCListen)
	if err != nil {
		return fmt.Errorf("listen for Redfish BMC on %s: %w", cfg.BMCListen, err)
	}

	server := &http.Server{
		Handler:           NewRedfishHandler(vm, cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	done := make(chan error, 1)

	go func() {
		done <- server.ServeTLS(listener, cfg.BMCCertPath, cfg.BMCKeyPath)
	}()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("serve Redfish BMC: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), bmcShutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down Redfish BMC: %w", err)
		}

		err := <-done
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve Redfish BMC: %w", err)
		}

		return nil
	}
}
