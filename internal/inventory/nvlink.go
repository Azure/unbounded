// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// IMEXNodeAttributes holds the parsed fields for a single IMEX domain peer.
type IMEXNodeAttributes struct {
	NodeIndex int    `json:"node_index"`
	IPAddress string `json:"ip_address"`
	Status    string `json:"status"`
	Version   string `json:"version"`
	Hostname  string `json:"hostname"`
	IsLocal   bool   `json:"is_local"`
}

// imexNodeLine matches lines like:
//
//	Node #4   * 180.100.41.232 *   - READY   - Version: 580.95.09   - Hostname: MKE030104700131
//	Node #0   - 180.100.41.183     - READY   - Version: 580.95.09   - Hostname: MKE030104700112
var imexNodeLine = regexp.MustCompile(
	`^Node\s+#(\d+)\s+([*-])\s+(\S+)\s+[*\s]+-\s+(\S+)\s+-\s+Version:\s*(.*?)\s+-\s+Hostname:\s*(\S*)`,
)

// collectIMEXNeighbors runs `nvidia-imex-ctl -H` and parses the output into
// NeighborRecords for the neighbors table. Returns nil, nil if the command is
// not available.
func collectIMEXNeighbors(ctx context.Context, hostID string) ([]NeighborRecord, error) {
	path, err := exec.LookPath("nvidia-imex-ctl")
	if err != nil {
		return nil, nil // not installed, nothing to collect
	}

	out, err := exec.CommandContext(ctx, path, "-H").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-imex-ctl: %w", err)
	}

	return parseIMEXOutput(hostID, out)
}

// parseIMEXOutput parses raw `nvidia-imex-ctl -H` output into NeighborRecords.
// Separated from collectIMEXNeighbors for testability.
func parseIMEXOutput(hostID string, data []byte) ([]NeighborRecord, error) {
	var records []NeighborRecord

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		m := imexNodeLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		nodeIdx, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}

		attrs := IMEXNodeAttributes{
			NodeIndex: nodeIdx,
			IPAddress: m[3],
			Status:    m[4],
			Version:   strings.TrimSpace(m[5]),
			Hostname:  m[6],
			IsLocal:   m[2] == "*",
		}

		records = append(records, NeighborRecord{
			HostIdentifier: hostID,
			LocalInterface: fmt.Sprintf("imex-domain/node%d", nodeIdx),
			Attributes:     mustMarshal(attrs),
		})
	}

	return records, scanner.Err()
}

// printIMEXNeighbors prints discovered IMEX peers to stdout.
func printIMEXNeighbors(neighbors []NeighborRecord) {
	fmt.Printf("IMEX Domain Peers Found: %d\n", len(neighbors))

	for i, n := range neighbors {
		var attrs IMEXNodeAttributes

		if err := json.Unmarshal(n.Attributes, &attrs); err != nil {
			continue
		}

		marker := " "
		if attrs.IsLocal {
			marker = "*"
		}

		fmt.Printf("  %sNode %d (%s): %s  %-13s  version=%s  host=%s\n",
			marker, i, attrs.IPAddress, attrs.Status, "", attrs.Version, attrs.Hostname)
	}
}
