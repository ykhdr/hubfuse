package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ykhdr/hubfuse/internal/hub/store"
)

// StopOutcome reports how far the bounded gRPC shutdown got. Hub.Stop uses it
// to decide whether the store may still be closed underneath live handlers —
// closing it on anything but StopGraceful hands a running handler
// "sql: database is closed" instead of the clean disconnect it would otherwise
// get from the process exiting with its WAL already committed. (#75)
type StopOutcome int

const (
	// StopGraceful means GracefulStop returned on its own inside grace: every
	// handler has already returned, so the store is safe to close.
	StopGraceful StopOutcome = iota
	// StopForced means grace expired and the server was forced down with
	// Stop(), which returned inside hardLimit. Handlers may still be running
	// (grpc-go server.go:1985-1986 only waits for them on the graceful path),
	// so the store must stay open.
	StopForced
	// StopHung means even the forced Stop() did not return inside hardLimit.
	// StopServer returns anyway; the caller is expected to exit the process
	// rather than keep waiting.
	StopHung
)

// String renders o for logging.
func (o StopOutcome) String() string {
	switch o {
	case StopGraceful:
		return "graceful"
	case StopForced:
		return "forced"
	case StopHung:
		return "hung"
	default:
		return "unknown"
	}
}

// stoppableServer is the slice of *grpc.Server that StopServer needs. It is an
// interface, not *grpc.Server, so a test can supply a fake with controllable
// timing instead of driving a real listener through the same clock as the
// hard limit it is meant to bound. The real *grpc.Server satisfies it as-is.
// (#75)
type stoppableServer interface {
	GracefulStop()
	Stop()
}

const (
	// DefaultShutdownBudget is the upper bound the hub's own shutdown sequence
	// allows itself, StopServer's grace and hardLimit included. It is exported
	// because the scenario-test harness has to wait longer than this before it
	// treats the hub as hung and reaches for SIGKILL — deriving that wait from
	// this constant is what keeps the two from drifting apart; a second
	// hardcoded literal there would not notice this one changing. (#75)
	DefaultShutdownBudget = 6 * time.Second

	// shutdownGrace is how long StopServer waits for GracefulStop before
	// forcing the server down with Stop(). It is set below grpc-go's own
	// hardcoded 5-second timer between the two GOAWAY frames a graceful
	// shutdown sends (http2_server.go:1414-1421, the second GOAWAY is what
	// actually drops a connection) so a dead peer is forced down before the hub
	// pays for grpc's own wait rather than racing it to the same place. The
	// outer frame this has to fit inside is daemonize's 10-second
	// stopGracefulTimeout before SIGKILL, shared with the sweep and
	// CloseAllSubscribers that run before StopServer is even called. (#75)
	shutdownGrace = 3 * time.Second
)

// StopServer stops srv with a hard upper bound and reports how far it got.
//
// GracefulStop and Stop both block until every connection has gone away
// (grpc-go server.go:1952-1953 — quit fires immediately, done only after the
// connection wait), so calling either synchronously here would itself consume
// the budget this function exists to enforce. Both therefore run in their own
// goroutines, and StopServer only ever blocks on timers and on the buffered
// channel those goroutines report through — a goroutine whose send arrives
// after StopServer has already returned (the StopHung case) must not block
// forever on it. (#75)
func StopServer(srv stoppableServer, grace, hardLimit time.Duration, logger *slog.Logger) StopOutcome {
	done := make(chan struct{}, 1)
	go func() {
		srv.GracefulStop()
		done <- struct{}{}
	}()

	select {
	case <-done:
		if logger != nil {
			logger.Info("gRPC server stopped gracefully")
		}
		return StopGraceful
	case <-time.After(grace):
	}

	// Grace expired: at least one handler is still live — with a healthy
	// subscriber this is the ordinary case, not just a dead peer (see
	// CloseAllSubscribers). Stop() forces every connection closed, which is
	// what unblocks the GracefulStop goroutine above; it does not return here
	// because it finished on its own, it returns because Stop() emptied the
	// connection set out from under it. Calling Stop() concurrently with a
	// still-hanging GracefulStop is legal in grpc-go.
	if logger != nil {
		logger.Warn("gRPC graceful stop did not finish within grace, forcing",
			slog.Duration("grace", grace))
	}
	go srv.Stop()

	select {
	case <-done:
		if logger != nil {
			logger.Warn("gRPC server stopped forcibly")
		}
		return StopForced
	case <-time.After(hardLimit):
		// grpc.WaitForHandlers(true) is not the fix here: it turns this into an
		// unbounded wait, which is the exact failure StopServer exists to bound.
		// The caller is expected to exit the process instead.
		if logger != nil {
			logger.Warn("gRPC server did not stop even after a forced Stop",
				slog.Duration("hard_limit", hardLimit))
		}
		return StopHung
	}
}

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

// logCancelable logs err at level, except when err is a context cancellation
// or deadline expiry, in which case it logs at Debug instead.
//
// Stop cancels the background goroutines' context as step 2 of the shutdown
// sequence, and every store call still in flight at that moment — a stale
// heartbeat sweep, an invite prune, a join-token sweep — returns exactly this
// error as a direct consequence, not as evidence of a store or connectivity
// problem. Logging it at the caller's normal level would turn every clean
// shutdown into an Error/Warn line indistinguishable from a real fault; every
// other error keeps its usual level. attrs are appended after "error" so
// call sites keep whatever extra context (e.g. device_id) they already had.
// (#75)
func logCancelable(logger *slog.Logger, level slog.Level, msg string, err error, attrs ...slog.Attr) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		level = slog.LevelDebug
	}
	all := append([]slog.Attr{slog.Any("error", err)}, attrs...)
	logger.LogAttrs(context.Background(), level, msg, all...)
}
