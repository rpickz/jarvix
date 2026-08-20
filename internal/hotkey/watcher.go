package hotkey

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// keyboardGlobs locate keyboard event devices. by-path and by-id both list
// keyboards; entries resolving to the same eventN are deduplicated.
var keyboardGlobs = []string{
	"/dev/input/by-path/*-event-kbd",
	"/dev/input/by-id/*-event-kbd",
}

// Accessible reports whether at least one keyboard device is readable —
// used by the daemon to decide whether daemon-side PTT can run, and by
// jarvix doctor to explain how to grant access.
func Accessible() bool {
	for _, dev := range listKeyboards() {
		f, err := os.Open(dev)
		if err == nil {
			f.Close()
			return true
		}
	}
	return false
}

func listKeyboards() []string {
	seen := map[string]bool{}
	var devices []string
	for _, glob := range keyboardGlobs {
		matches, _ := filepath.Glob(glob)
		for _, link := range matches {
			real, err := filepath.EvalSymlinks(link)
			if err != nil || seen[real] {
				continue
			}
			seen[real] = true
			devices = append(devices, real)
		}
	}
	return devices
}

// Watcher reads keyboards and drives a ChordTracker. Hotplug is handled by
// rescanning; a device that disappears just ends its reader.
type Watcher struct {
	tracker *ChordTracker
	log     *slog.Logger

	mu   sync.Mutex
	open map[string]bool
}

// NewWatcher builds a watcher. Callbacks are invoked on the watcher's event
// goroutine and must not block.
func NewWatcher(codes []uint16, onPress, onRelease func(), logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	t := NewChordTracker(codes)
	t.OnPress = onPress
	t.OnRelease = onRelease
	return &Watcher{tracker: t, log: logger, open: make(map[string]bool)}
}

// Run watches keyboards until ctx is cancelled. Events from all devices are
// serialised through one channel so the tracker needs no locking.
func (w *Watcher) Run(ctx context.Context) {
	events := make(chan keyEvent, 64)
	go func() {
		for {
			select {
			case ev := <-events:
				w.tracker.Handle(ev)
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		for _, dev := range listKeyboards() {
			w.mu.Lock()
			already := w.open[dev]
			if !already {
				w.open[dev] = true
			}
			w.mu.Unlock()
			if !already {
				go w.readDevice(ctx, dev, events)
			}
		}
		select {
		case <-time.After(5 * time.Second): // hotplug rescan
		case <-ctx.Done():
			return
		}
	}
}

func (w *Watcher) readDevice(ctx context.Context, dev string, events chan<- keyEvent) {
	defer func() {
		w.mu.Lock()
		delete(w.open, dev)
		w.mu.Unlock()
	}()

	f, err := os.Open(dev)
	if err != nil {
		// Logged at debug: without input access this fires for every device
		// on every rescan, and doctor already explains the fix loudly.
		w.log.Debug("cannot open input device", "component", "hotkey", "device", dev, "error", err.Error())
		return
	}
	defer f.Close()
	go func() { // unblock the Read below on shutdown
		<-ctx.Done()
		f.Close()
	}()
	w.log.Info("watching keyboard", "component", "hotkey", "device", dev)

	buf := make([]byte, eventSize*32)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Debug("input device closed", "component", "hotkey", "device", dev)
			}
			return
		}
		for _, ev := range decodeEvents(buf[:n]) {
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}
}
