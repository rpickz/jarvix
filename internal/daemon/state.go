package daemon

// This file is the backup IPC surface (ADR 0045): state.hold and
// state.release, the verbs `jarvix backup` uses to copy a coherent state
// root out from under a running daemon.
//
// state.hold closes the write barrier every daemon-owned store enters around
// each disk mutation (internal/statehold): in-flight writes are drained
// first, new ones block until release. The reply arriving IS the guarantee —
// from that moment until state.release (or the TTL expiring, whichever comes
// first) nothing under the state root moves. The TTL is not a courtesy: a
// backup process that dies mid-copy must never leave an assistant that can
// hear but not remember, so the gate always reopens on its own.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/statehold"
)

// Hold bounds. The default is generous for a state dir of small files; the
// maximum exists so a buggy caller cannot park the daemon's writes for
// minutes by asking nicely.
const (
	// DefaultHoldTTL is used when state.hold names no ttl_ms.
	DefaultHoldTTL = 10 * time.Second
	// MaxHoldTTL caps what a caller may request.
	MaxHoldTTL = 60 * time.Second
	// holdDrainGrace bounds the wait for in-flight writes to settle before
	// the hold is refused: a wedged disk must fail the backup with a reason,
	// never hang the verb.
	holdDrainGrace = 5 * time.Second
)

func (d *Daemon) registerStateMethods() {
	d.server.Handle("state.hold", d.handleStateHold)
	d.server.Handle("state.release", d.handleStateRelease)
}

// handleStateHold closes the write barrier and reports how long it will stay
// closed unless released. A second hold while one is active is refused — one
// backup at a time; the first to finish releases.
func (d *Daemon) handleStateHold(params json.RawMessage) (any, error) {
	var req struct {
		TTLMs int `json:"ttl_ms"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "malformed params: %v", err)
		}
	}
	ttl := DefaultHoldTTL
	if req.TTLMs != 0 {
		ttl = time.Duration(req.TTLMs) * time.Millisecond
		if ttl < 0 || ttl > MaxHoldTTL {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"ttl_ms must be between 0 and %d", MaxHoldTTL.Milliseconds())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), holdDrainGrace)
	defer cancel()
	release, err := d.stateGate.Hold(ctx, ttl)
	if err != nil {
		if errors.Is(err, statehold.ErrHeld) {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest,
				"state writes are already held — another backup is running")
		}
		return nil, err
	}
	d.holdMu.Lock()
	d.holdRelease = release
	d.holdMu.Unlock()
	d.log.Info("state writes held for backup", "component", "state", "ttl", ttl)
	return map[string]any{"held": true, "ttl_ms": ttl.Milliseconds()}, nil
}

// handleStateRelease reopens the barrier. Idempotent on purpose: releasing
// after the TTL already fired reports released all the same — the caller's
// question is "may writes flow again?", and the answer is yes either way.
func (d *Daemon) handleStateRelease(json.RawMessage) (any, error) {
	d.holdMu.Lock()
	release := d.holdRelease
	d.holdRelease = nil
	d.holdMu.Unlock()
	if release != nil {
		release()
		d.log.Info("state writes released", "component", "state")
	}
	return map[string]any{"held": false}, nil
}
