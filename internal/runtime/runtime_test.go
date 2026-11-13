package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/inputs"
)

// fakeSensor counts its runs, fails the first failures of them, and hands the
// wake it was given to the test.
type fakeSensor struct {
	inputs.Latest
	runs      atomic.Int32
	failures  int32
	returnNil bool
	wake      chan inputs.WakeFunc
}

func newFakeSensor() *fakeSensor { return &fakeSensor{wake: make(chan inputs.WakeFunc, 16)} }

func (s *fakeSensor) Name() string { return "fake" }

func (s *fakeSensor) Run(ctx context.Context, wake inputs.WakeFunc) error {
	if n := s.runs.Add(1); n <= s.failures {
		if s.returnNil {
			return nil
		}
		return errors.New("boom")
	}
	s.wake <- wake
	<-ctx.Done()
	return ctx.Err()
}

type joinFuser struct{}

func (joinFuser) Fuse(_ time.Time, obs []string) string { return strings.Join(obs, "|") }

func testOptions() Options {
	return Options{Hertz: 0.01, BackoffMin: time.Millisecond, BackoffMax: 4 * time.Millisecond, HealthyAfter: time.Hour}
}

// start runs a Runtime and returns a stop func that fails the test if Run does
// not return promptly.
func start(t *testing.T, sensors []inputs.Sensor, opts Options) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	rt := New(sensors, joinFuser{}, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	go func() { done <- rt.Run(ctx) }()
	return func() {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v, want nil", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Run did not return within 1s of cancel")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type tickLog struct {
	mu    sync.Mutex
	ticks []Tick
}

func (l *tickLog) add(t Tick) { l.mu.Lock(); l.ticks = append(l.ticks, t); l.mu.Unlock() }
func (l *tickLog) all() []Tick {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Tick(nil), l.ticks...)
}

func TestTimerTicks(t *testing.T) {
	s := newFakeSensor()
	s.Set("hello")
	var log tickLog
	opts := testOptions()
	opts.Hertz, opts.OnTick = 100, log.add
	stop := start(t, []inputs.Sensor{s}, opts)
	waitFor(t, "5 timer ticks", func() bool { return len(log.all()) >= 5 })
	stop()

	for i, tk := range log.all() {
		if tk.Trigger != "timer" || tk.Reasons != nil || tk.Prompt != "fake: hello" || tk.N != uint64(i+1) {
			t.Fatalf("tick %d = %+v", i, tk)
		}
	}
}

func TestWakesAreMerged(t *testing.T) {
	s := newFakeSensor()
	var log tickLog
	release := make(chan struct{})
	opts := testOptions()
	opts.OnTick = func(tk Tick) {
		log.add(tk)
		if tk.N == 1 {
			<-release // hold tick 1 while more wakes arrive
		}
	}
	stop := start(t, []inputs.Sensor{s}, opts)
	defer stop()

	wake := <-s.wake
	wake("first")
	waitFor(t, "tick 1", func() bool { return len(log.all()) == 1 })
	for i := range 20 {
		wake(fmt.Sprintf("r%d", i))
	}
	close(release)

	waitFor(t, "tick 2", func() bool { return len(log.all()) == 2 })
	time.Sleep(30 * time.Millisecond)
	ticks := log.all()
	if len(ticks) != 2 {
		t.Fatalf("got %d ticks, want 2: one wake gives one tick, 20 merged wakes give one more", len(ticks))
	}
	if got := ticks[0]; got.Trigger != "wake" || len(got.Reasons) != 1 || got.Reasons[0] != "first" {
		t.Errorf("tick 1 = %+v", got)
	}
	if got := ticks[1]; got.Trigger != "wake" || len(got.Reasons) != 20 {
		t.Errorf("tick 2 carries %d reasons, want all 20", len(got.Reasons))
	}
}

func TestDecideSkipsIdleTicks(t *testing.T) {
	s := newFakeSensor()
	var ticks, decided atomic.Int32
	var wakeDecided atomic.Bool
	opts := testOptions()
	opts.Hertz, opts.DecideEvery = 200, 4
	opts.OnTick = func(Tick) { ticks.Add(1) }
	opts.Decide = func(_ context.Context, tk Tick) {
		decided.Add(1)
		if tk.Trigger == "wake" {
			wakeDecided.Store(true)
		}
	}
	stop := start(t, []inputs.Sensor{s}, opts)

	waitFor(t, "12 ticks", func() bool { return ticks.Load() >= 12 })
	if d, n := decided.Load(), ticks.Load(); d < 2 || d > n/4+1 {
		t.Errorf("Decide ran %d times in %d timer ticks, want one in four", d, n)
	}
	(<-s.wake)("event")
	waitFor(t, "Decide on the wake tick", wakeDecided.Load)
	stop()
}

func TestDecideWakeOnly(t *testing.T) {
	s := newFakeSensor()
	var ticks, decided atomic.Int32
	opts := testOptions()
	opts.Hertz = 200
	opts.OnTick = func(Tick) { ticks.Add(1) }
	opts.Decide = func(context.Context, Tick) { decided.Add(1) }
	stop := start(t, []inputs.Sensor{s}, opts)
	waitFor(t, "8 ticks", func() bool { return ticks.Load() >= 8 })
	stop()
	if d := decided.Load(); d != 0 {
		t.Errorf("Decide ran %d times on timer ticks with DecideEvery 0, want 0", d)
	}
}

func TestSensorIsRestarted(t *testing.T) {
	for _, returnNil := range []bool{false, true} {
		s := newFakeSensor()
		s.failures, s.returnNil = 2, returnNil
		stop := start(t, []inputs.Sensor{s}, testOptions())
		waitFor(t, "third run", func() bool { return s.runs.Load() >= 3 })
		stop()
	}
}

func TestWakeAfterShutdown(t *testing.T) {
	s := newFakeSensor()
	stop := start(t, []inputs.Sensor{s}, testOptions())
	wake := <-s.wake
	stop()
	wake("late") // must neither block nor panic
	wake("later")
}
