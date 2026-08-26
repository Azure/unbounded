// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lib

import "fmt"

// Boxing Widget into an interface is what makes RTA treat every exported
// method on it as reachable through reflection. This tool must not care.
var _ fmt.Stringer = Widget{}
