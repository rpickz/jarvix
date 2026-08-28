package daemon

import (
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// testConfig is config.Default() with desktop context switched off. Every
// daemon test uses it instead of the defaults, because the defaults enable
// window and selection capture — and a test suite must never run hyprctl or
// read the clipboard of whoever happens to be running it. Context has its own
// tests in internal/desktop and internal/session, and the mapping from
// configuration to collector is asserted below.
func testConfig() config.Config {
	cfg := config.Default()
	cfg.Context.Window = false
	cfg.Context.Selection = false
	cfg.Context.Clipboard = false
	return cfg
}

func TestContextCollectorIsAbsentWhenEverySourceIsOff(t *testing.T) {
	// Not "a collector that gathers nothing" — absent. A typed-nil collector
	// in the interface would be non-nil to the engine and gather on every
	// turn, which is exactly the bug this asserts against.
	if got := contextCollector(testConfig(), nil); got != nil {
		t.Fatalf("collector = %#v, want nil with context disabled", got)
	}
	if opts := engineOptions(testConfig(), nil, nil, nil, nil, nil, nil, nil, nil, nil); opts.Context != nil {
		t.Errorf("engine options carried a collector: %#v", opts.Context)
	}
}

func TestContextCollectorIsBuiltFromConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Context.Clipboard = true
	if got := contextCollector(cfg, nil); got == nil {
		t.Fatal("no collector built for an enabled source")
	}
	if opts := engineOptions(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil); opts.Context == nil {
		t.Error("engine options carried no collector for an enabled source")
	}
}

func TestContextLastReportsNothingBeforeAnyCapture(t *testing.T) {
	client, _ := startDaemon(t)
	var last map[string]any
	if err := client.Call("context.last", nil, &last); err != nil {
		t.Fatal(err)
	}
	if last["captured"] != false {
		t.Errorf("context.last = %v, want captured: false", last)
	}
}
