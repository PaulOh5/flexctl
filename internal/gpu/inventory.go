package gpu

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Sync upserts the host's current GPU set into gpu_inventory.
//
// We key on UUID so a host reboot's device-index reorder does not
// invalidate scheduler bookkeeping (Eng review #7). last_seen_at is
// always refreshed; device_index and name are kept current in case the
// host swaps cards.
//
// Returns the count of devices written. If nvidia-smi is unavailable
// (developer laptop, CI without GPUs), Sync returns 0 with a non-fatal
// error so callers can warn-and-continue.
func Sync(ctx context.Context, store *sql.DB) (int, error) {
	devs, err := List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list devices: %w", err)
	}

	now := time.Now().Unix()
	tx, err := store.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	for _, d := range devs {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO gpu_inventory (uuid, device_index, name, memory_total_mb, last_seen_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(uuid) DO UPDATE SET
			    device_index    = excluded.device_index,
			    name            = excluded.name,
			    memory_total_mb = excluded.memory_total_mb,
			    last_seen_at    = excluded.last_seen_at
		`, d.UUID, d.DeviceIndex, d.Name, d.MemoryTotalMB, now)
		if err != nil {
			return 0, fmt.Errorf("upsert %s: %w", d.UUID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(devs), nil
}
