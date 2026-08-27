// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command app is the fixture's only binary. It exists so the fixture has a
// live reference to some of lib and not to the rest.
package main

import (
	"fmt"

	"example.com/simple/lib"
)

func main() {
	w := lib.Used()

	fmt.Println(w.Referenced(), lib.UsedConst)
}
