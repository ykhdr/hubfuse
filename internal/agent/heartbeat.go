package agent

import (
	"context"
	"time"
)

// runHeartbeat sends a Heartbeat RPC to the hub every 10 seconds until ctx is
// cancelled. Transient errors are logged as warnings but do not stop the loop.
func (d *Daemon) runHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.hubClient.Heartbeat(ctx); err != nil {
				d.logger.Warn("heartbeat failed", "error", err)
			}
		}
	}
}
