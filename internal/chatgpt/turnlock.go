package chatgpt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Cross-process turn lock. The :8788 adapter and the MCP server are separate
// processes driving ONE Chrome, so in-process guards (c.mu, the adapter's
// gate) cannot see each other: an MCP ask landing mid-turn interleaves
// navigate/type/poll with the adapter's live thread (and vice versa). This
// file lock — one per Chrome profile dir — makes whole browser turns
// mutually exclusive across both binaries. It is held for the full turn
// (first navigate through answer capture), never for read-only probes.
//
// The kernel releases the lock when the holder's fd closes, so a killed
// process cannot wedge the other side into a stale lock. Blocking wait is
// context-aware: client disconnect aborts the wait instead of queueing
// forever behind a 10-minute turn.
func LockTurn(ctx context.Context, profileDir string) (unlock func(), err error) {
	// Empty dir means single-process mode (unit tests, fake backends):
	// nothing cross-process to coordinate with, so no lock. Production
	// binaries always pass the real Chrome profile dir.
	if profileDir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, fmt.Errorf("turn lock dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(profileDir, "bridge.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("turn lock open: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		if ferr := tryTurnLock(f); ferr == nil {
			return func() { _ = f.Close() }, nil
		} else if !turnLockContended(ferr) {
			_ = f.Close()
			return nil, fmt.Errorf("turn lock acquire: %w", ferr)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
