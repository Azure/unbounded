package utilcli

import (
	"fmt"
	"strings"
)

// Example formats the given string as a command example.
// It replaces the %[1]s placeholder with the command name "kubectl unbounded".
func Example(s string) string {
	return strings.Trim(fmt.Sprintf(s, "kubectl unbounded"), "\n")
}
