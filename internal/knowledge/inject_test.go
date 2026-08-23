package knowledge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The injection block shares the memory block's budget discipline (ADR
// 0025): measured, capped, trims disclosed — and it must never fetch,
// because it runs on the turn path.

func injectService(t *testing.T, clock *fakeClock, runner *stubRunner, capTokens int, feeds ...Feed) *Service {
	t.Helper()
	return NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds:             feeds,
		MaxInjectedTokens: capTokens,
		RefreshAllowed:    true,
		Now:               clock.Now,
		Runner:            runner.run,
	}, testLogger(t))
}

func TestInjectCarriesOptedInValuesWithSpokenAge(t *testing.T) {
	clock := newFakeClock()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"))
	runner.script("weather", ok("light rain"))
	amd := lazyFeed("amd")
	amd.Inject = true
	amd.Description = "AMD share price in dollars"
	weather := lazyFeed("weather") // not opted in
	s := injectService(t, clock, runner, 300, amd, weather)
	defer drain(t, s)
	s.Get(context.Background(), "amd")
	s.Get(context.Background(), "weather")

	clock.Advance(4 * time.Minute)
	inj := s.Inject()
	if inj.Feeds != 1 || inj.Trimmed != 0 {
		t.Fatalf("injection = %+v, want exactly the one opted-in feed", inj)
	}
	if !strings.Contains(inj.Message, "amd (AMD share price in dollars): 187.42") {
		t.Errorf("message lacks the value line:\n%s", inj.Message)
	}
	if !strings.Contains(inj.Message, "as of four minutes ago") {
		t.Errorf("message lacks the spoken age:\n%s", inj.Message)
	}
	if strings.Contains(inj.Message, "light rain") {
		t.Errorf("a feed that did not opt in was injected:\n%s", inj.Message)
	}
	if inj.EstTokens <= 0 || inj.EstTokens > 300 {
		t.Errorf("est_tokens = %d, want a positive estimate within the cap", inj.EstTokens)
	}
}

func TestInjectDisclosesStaleness(t *testing.T) {
	clock := newFakeClock()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"), failed(1))
	amd := lazyFeed("amd")
	amd.Inject = true
	s := injectService(t, clock, runner, 300, amd)
	defer drain(t, s)
	s.Get(context.Background(), "amd")

	clock.Advance(11 * time.Minute)    // past the 10m ttl
	s.Get(context.Background(), "amd") // the refetch fails; last good stands
	if inj := s.Inject(); !strings.Contains(inj.Message, "stale") {
		t.Errorf("a stale value was injected without saying so:\n%s", inj.Message)
	}
}

func TestInjectNeverFetches(t *testing.T) {
	clock := newFakeClock()
	amd := lazyFeed("amd")
	amd.Inject = true
	s := NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds:          []Feed{amd},
		RefreshAllowed: true,
		Now:            clock.Now,
		Runner: func(context.Context, Feed, []string) FetchResult {
			t.Error("Inject ran a fetch; a model turn must never wait on a feed command")
			return FetchResult{}
		},
	}, testLogger(t))
	defer drain(t, s)
	if inj := s.Inject(); inj.Message != "" || inj.Feeds != 0 {
		t.Errorf("injection with an empty cache = %+v, want nothing", inj)
	}
}

func TestInjectTrimsAtTheBudgetAndDiscloses(t *testing.T) {
	clock := newFakeClock()
	runner := newStubRunner()
	feeds := make([]Feed, 0, 3)
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		f := lazyFeed(name)
		f.Inject = true
		f.Description = "the " + name + " feed"
		feeds = append(feeds, f)
		runner.script(name, ok(strings.Repeat(name+" ", 20)))
	}
	// A cap that fits the preamble and roughly one entry, so the tail of the
	// declaration order is what gets trimmed.
	const capTokens = 145
	s := injectService(t, clock, runner, capTokens, feeds...)
	defer drain(t, s)
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		s.Get(context.Background(), name)
	}

	inj := s.Inject()
	if inj.Feeds == 0 || inj.Trimmed == 0 || inj.Feeds+inj.Trimmed != 3 {
		t.Fatalf("injection = %+v, want some kept and some trimmed of 3", inj)
	}
	if !strings.Contains(inj.Message, "alpha") {
		t.Errorf("declaration order was not respected — the first feed was trimmed:\n%s", inj.Message)
	}
	if !strings.Contains(inj.Message, "left out to save space") ||
		!strings.Contains(inj.Message, "knowledge.get") {
		t.Errorf("trim was not disclosed with the recovery tool named:\n%s", inj.Message)
	}
	if inj.EstTokens > capTokens {
		t.Errorf("est_tokens = %d, exceeds the stated cap", inj.EstTokens)
	}
}
