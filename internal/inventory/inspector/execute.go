// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inspector

import (
	"context"
	"fmt"
)

// Config holds the configuration for the inventory inspector.
type Config struct {
	Debug bool
}

// Execute runs the inventory inspector.
func Execute(ctx context.Context, config Config) error {
	if config.Debug {
		fmt.Println("Running in debug mode")
	}

	fmt.Println("inventory-inspector: not yet implemented")

	return nil
}
