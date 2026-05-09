// Package gpu enumerates GPUs on the host via nvidia-smi.
//
// We deliberately shell out instead of pulling in pynvml-equivalent
// bindings: we only need a small slice of the data, and shelling out
// keeps the binary fully static and easy to reason about.
package gpu

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Device describes one GPU on the host as observed by nvidia-smi.
type Device struct {
	UUID          string
	DeviceIndex   int
	Name          string
	MemoryTotalMB int
}

// List runs `nvidia-smi --query-gpu=...` and parses the CSV.
func List(ctx context.Context) ([]Device, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "nvidia-smi",
		"--query-gpu=uuid,index,name,memory.total",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}

	r := csv.NewReader(strings.NewReader(string(out)))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = 4

	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse csv: %w", err)
	}

	devs := make([]Device, 0, len(rows))
	for _, row := range rows {
		idx, err := strconv.Atoi(strings.TrimSpace(row[1]))
		if err != nil {
			return nil, fmt.Errorf("bad index %q: %w", row[1], err)
		}
		mem, err := strconv.Atoi(strings.TrimSpace(row[3]))
		if err != nil {
			return nil, fmt.Errorf("bad memory %q: %w", row[3], err)
		}
		devs = append(devs, Device{
			UUID:          strings.TrimSpace(row[0]),
			DeviceIndex:   idx,
			Name:          strings.TrimSpace(row[2]),
			MemoryTotalMB: mem,
		})
	}
	return devs, nil
}
