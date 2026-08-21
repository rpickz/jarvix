package warm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeChild stands in for a warm engine process. Nothing here spawns anything:
// the supervisor's job is lifecycle, and lifecycle is what these tests pin.
type fakeChild struct {
	pid    int
	closed atomic.Bool
	gone   chan struct{}
}

func newFakeChild(pid int) *fakeChild { return &fakeChild{pid: pid, gone: make(chan struct{})} }

func (c *fakeChild) PID() int { return c.pid }

func (c *fakeChild) Close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.gone)
	}
}

// awaitClosed waits for the supervisor's asynchronous Close to land. Closing
// happens off the lock (a real child is killed and waited on), so tests
// synchronise on the child itself rather than on elapsed time.
func (c *fakeChild) awaitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-c.gone:
	case <-time.After(2 * time.Second):
		t.Fatalf("child %d was never closed", c.pid)
	}
}

// spawner hands out pre-made children and counts how often it was asked.
type spawner struct {
	mu       sync.Mutex
	children []*fakeChild
	err      error
	calls    int
}

func (s *spawner) spawn(context.Context) (*fakeChild, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	child := newFakeChild(1000 + s.calls)
	s.children = append(s.children, child)
	return child, nil
}

func (s *spawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestGetReusesTheWarmChild(t *testing.T) {
	sp := &spawner{}
	s := &Supervisor[*fakeChild]{Name: "test", Spawn: sp.spawn, Log: discardLogger()}
	defer s.Close()

	first, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Release()
	second, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("second Get returned a different child; the model would reload")
	}
	if sp.count() != 1 {
		t.Errorf("spawned %d times, want 1", sp.count())
	}
}

func TestDiscardRestartsOnTheNextGet(t *testing.T) {
	sp := &spawner{}
	s := &Supervisor[*fakeChild]{Name: "test", Spawn: sp.spawn, Log: discardLogger()}
	defer s.Close()

	first, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Discard("engine crashed")
	first.awaitClosed(t)

	second, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("a discarded child was handed out again")
	}
	if st := s.Status(); st.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", st.Restarts)
	}
}

func TestFailedSpawnBacksOffThenRetries(t *testing.T) {
	sp := &spawner{err: fmt.Errorf("engine not installed")}
	now := time.Now()
	s := &Supervisor[*fakeChild]{
		Name: "test", Spawn: sp.spawn, Log: discardLogger(),
		Now: func() time.Time { return now },
	}
	defer s.Close()

	if _, err := s.Get(context.Background()); err == nil {
		t.Fatal("a failing spawn must report an error so the caller runs cold")
	}
	// Inside the backoff window the supervisor must not spawn again: a machine
	// without the engine would otherwise fork per interaction.
	if _, err := s.Get(context.Background()); err == nil {
		t.Fatal("want an error during backoff")
	}
	if sp.count() != 1 {
		t.Fatalf("spawn attempts during backoff = %d, want 1", sp.count())
	}

	// Past the window, and now healthy, the next Get succeeds.
	now = now.Add(time.Minute)
	sp.mu.Lock()
	sp.err = nil
	sp.mu.Unlock()
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatalf("Get after the backoff window: %v", err)
	}
	if sp.count() != 2 {
		t.Errorf("spawn attempts = %d, want 2", sp.count())
	}
}

func TestMemoryCapRetiresTheChildBeforeItIsUsedAgain(t *testing.T) {
	sp := &spawner{}
	var rss atomic.Uint64
	rss.Store(100 << 20)
	s := &Supervisor[*fakeChild]{
		Name: "test", Spawn: sp.spawn, Log: discardLogger(),
		MemoryCap: 512 << 20,
		RSS:       func(int) (uint64, error) { return rss.Load(), nil },
	}
	defer s.Close()

	first, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rss.Store(600 << 20) // the engine leaked past its cap
	second, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("a child over the memory cap was handed out again")
	}
	first.awaitClosed(t)
	if st := s.Status(); st.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", st.Restarts)
	}
}

func TestUnreadableMemoryDoesNotKillAWorkingChild(t *testing.T) {
	sp := &spawner{}
	s := &Supervisor[*fakeChild]{
		Name: "test", Spawn: sp.spawn, Log: discardLogger(),
		MemoryCap: 1 << 20,
		RSS:       func(int) (uint64, error) { return 0, fmt.Errorf("no /proc entry") },
	}
	defer s.Close()

	first, _ := s.Get(context.Background())
	second, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Error("an unreadable RSS must not be treated as a cap breach")
	}
}

func TestIdleReapFreesTheChildAndTheNextGetPaysACOldStart(t *testing.T) {
	sp := &spawner{}
	fire := make(chan time.Time, 1)
	s := &Supervisor[*fakeChild]{
		Name: "test", Spawn: sp.spawn, Log: discardLogger(),
		IdleAfter: time.Hour,
		Timer:     func(time.Duration) (<-chan time.Time, func()) { return fire, func() {} },
	}
	defer s.Close()

	first, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Release() // arms the reaper

	fire <- time.Now() // the idle period lapsed
	first.awaitClosed(t)

	second, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("a reaped child was handed out again")
	}
	if sp.count() != 2 {
		t.Errorf("spawns = %d, want 2 (the next session pays a cold start)", sp.count())
	}
}

func TestUseCancelsAnArmedReaper(t *testing.T) {
	sp := &spawner{}
	stopped := make(chan struct{}, 1)
	s := &Supervisor[*fakeChild]{
		Name: "test", Spawn: sp.spawn, Log: discardLogger(),
		IdleAfter: time.Hour,
		Timer: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {
				select {
				case stopped <- struct{}{}:
				default:
				}
			}
		},
	}
	defer s.Close()

	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Release()
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("using the worker must disarm the idle reaper")
	}
}

func TestCloseKillsTheChildAndRefusesFurtherGets(t *testing.T) {
	sp := &spawner{}
	s := &Supervisor[*fakeChild]{Name: "test", Spawn: sp.spawn, Log: discardLogger()}

	child, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	child.awaitClosed(t)

	if _, err := s.Get(context.Background()); err == nil {
		t.Error("Get after Close must fail rather than spawn an orphan")
	}
	if sp.count() != 1 {
		t.Errorf("spawns after Close = %d, want 1", sp.count())
	}
}

func TestStatusReportsTheLiveWorker(t *testing.T) {
	sp := &spawner{}
	s := &Supervisor[*fakeChild]{
		Name: "whisper", Spawn: sp.spawn, Log: discardLogger(),
		RSS: func(int) (uint64, error) { return 165 << 20, nil },
	}
	defer s.Close()

	if st := s.Status(); st.Running || st.Name != "whisper" {
		t.Fatalf("cold status = %+v", st)
	}
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if !st.Running || st.PID != 1001 || st.RSSBytes != 165<<20 {
		t.Errorf("warm status = %+v", st)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	if got := backoffFor(1); got != backoffBase {
		t.Errorf("backoffFor(1) = %v, want %v", got, backoffBase)
	}
	if got := backoffFor(2); got != 2*backoffBase {
		t.Errorf("backoffFor(2) = %v, want %v", got, 2*backoffBase)
	}
	if got := backoffFor(50); got != backoffMax {
		t.Errorf("backoffFor(50) = %v, want the cap %v", got, backoffMax)
	}
}

func TestResidentBytesReadsThisProcess(t *testing.T) {
	// Our own pid always has a /proc entry, so this pins the parser without a
	// fixture: a live process is never zero-resident.
	self, err := ResidentBytes(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if self == 0 {
		t.Error("this process reports zero resident bytes")
	}
	if _, err := ResidentBytes(-1); err == nil {
		t.Error("a pid with no /proc entry must report an error, not a size")
	}
}
