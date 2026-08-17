package hub

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ykhdr/hubfuse/internal/hub/store"
)

// ReconcileDeviceStatuses clears every online row before the hub serves
// anything. It runs at startup, and it is what makes the bounded shutdown safe.
//
// The hub trusts the statuses in its database across restarts: Register answers
// a joining device with ListOnlineDevices, so a stale online row is served to
// peers as a live endpoint to mount. Nothing clears those rows today. The
// shutdown sweep is the only writer that ever did, and it does not run at all
// after SIGKILL, OOM, or a power cut — and once shutdown is bounded by a budget,
// it may also be cut short on purpose. The heartbeat monitor eventually notices,
// but its first stale check is a full timeout/3 away (~10s by default), and it
// is the window before that one which peers spend mounting a dead device.
//
// So the invariant is established at the only moment it can be established
// cheaply and unconditionally: a hub that has just started has no online devices
// by definition — none of them has heartbeated yet. Everything alive re-registers
// within seconds and is announced again.
//
// It is a package-level function rather than a Hub method because the hub is
// built in two places — Hub.Start and hubtest — and a fix living in only one of
// them means the tests exercise a different lifecycle from production, the same
// reason ServerOptions exists. (#75)
func ReconcileDeviceStatuses(ctx context.Context, s store.Store, logger *slog.Logger) error {
	demoted, err := s.MarkAllOffline(ctx)
	if err != nil {
		return fmt.Errorf("reconcile device statuses: %w", err)
	}
	if demoted > 0 && logger != nil {
		// Not a warning: this is the expected state after any shutdown that did
		// not get to sweep, and after every SIGKILL.
		logger.Info("reconciled stale online devices at startup",
			slog.Int64("devices", demoted))
	}
	return nil
}
