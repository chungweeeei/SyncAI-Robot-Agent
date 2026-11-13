// Package runtime runs the agent's loop: sensors update in the background, and
// each tick fuses their latest texts into a prompt.
package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/inputs"
)

// PromptFuser turns the observations of one tick into a prompt.
type PromptFuser interface {
	Fuse(now time.Time, observations []string) string
}

// Tick is what one tick produced.
type Tick struct {
	N       uint64
	At      time.Time
	Trigger string   // "timer" or "wake"
	Reasons []string // the wake reasons this tick answers; nil for a timer tick
	Prompt  string
}

// Options configures the loop.
type Options struct {
	// Hertz is the base rate: the longest the loop idles between two ticks.
	Hertz float64

	// A sensor whose Run returns is restarted after a backoff that doubles from
	// BackoffMin to BackoffMax, and starts over only once a run has lasted
	// HealthyAfter: a sensor that dies at once must not restart every second.
	BackoffMin, BackoffMax, HealthyAfter time.Duration

	// OnTick is handed every tick, on the loop's goroutine. May be nil.
	OnTick func(Tick)

	// Decide is where a tick is acted on (the LLM call). It runs on the loop's
	// goroutine, so two never overlap, and wakes that arrive meanwhile are
	// merged into the one tick that follows. May be nil.
	Decide func(context.Context, Tick)

	// DecideEvery spaces Decide out over idle time: it runs on every wake tick
	// and on every DecideEvery-th timer tick since it last ran. 0 means wake
	// ticks only. Every Decide is an LLM call, so idle ticks are skipped.
	DecideEvery int
}

// DefaultOptions returns a 5 s base tick and a 1 s to 30 s restart backoff.
func DefaultOptions() Options {
	return Options{
		Hertz:        0.2,
		BackoffMin:   time.Second,
		BackoffMax:   30 * time.Second,
		HealthyAfter: time.Minute,
	}
}

// Runtime owns the loop and supervises the sensors.
type Runtime struct {
	sensors []inputs.Sensor
	fuser   PromptFuser
	log     *slog.Logger
	opts    Options

	// reasons and the signal on wakeCh are changed under mu together, so that
	// "a signal is pending" and "reasons are pending" are always the same thing.
	// Otherwise a reason is noticed one base tick late, or a base tick is
	// followed by an empty wake tick.
	mu      sync.Mutex
	reasons []string
	wakeCh  chan struct{}

	n    uint64
	idle int // timer ticks since Decide last ran
}

// New creates a Runtime. Sensors are rendered in the order given.
func New(sensors []inputs.Sensor, f PromptFuser, log *slog.Logger, opts Options) *Runtime {
	return &Runtime{sensors: sensors, fuser: f, log: log, opts: opts, wakeCh: make(chan struct{}, 1)}
}

// Run runs until ctx is canceled, then waits for every sensor to stop and
// returns nil.
func (r *Runtime) Run(ctx context.Context) error {
	if r.opts.Hertz <= 0 {
		return errors.New("runtime: hertz must be positive")
	}
	interval := time.Duration(float64(time.Second) / r.opts.Hertz)

	var wg sync.WaitGroup
	for _, s := range r.sensors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.supervise(ctx, s)
		}()
	}

	// One Timer reset after each tick, not a Ticker: a Ticker would fire a base
	// tick right behind a wake tick. No tick at start either; the sensors'
	// online wakes bring the first one.
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		trigger := "timer"
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-timer.C:
		case <-r.wakeCh:
			trigger = "wake"
		}
		r.tick(ctx, trigger, r.takeReasons())
		timer.Reset(interval)
	}
}

// wake is the inputs.WakeFunc handed to the sensors. It never blocks.
func (r *Runtime) wake(reason string) {
	r.mu.Lock()
	r.reasons = append(r.reasons, reason)
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
	r.mu.Unlock()
}

// takeReasons returns the pending reasons together with their signal.
func (r *Runtime) takeReasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	rs := r.reasons
	r.reasons = nil
	select {
	case <-r.wakeCh:
	default:
	}
	return rs
}

func (r *Runtime) tick(ctx context.Context, trigger string, reasons []string) {
	start := time.Now()
	if len(reasons) > 0 {
		// A timer tick that found reasons pending answers them.
		trigger = "wake"
	}

	// Every sensor is rendered against the same instant.
	obs := make([]string, 0, len(r.sensors))
	for _, s := range r.sensors {
		obs = append(obs, s.Snapshot().Render(s.Name(), start))
	}
	r.n++
	t := Tick{N: r.n, At: start, Trigger: trigger, Reasons: reasons, Prompt: r.fuser.Fuse(start, obs)}

	if r.opts.OnTick != nil {
		r.opts.OnTick(t)
	}

	decided := false
	if r.opts.Decide != nil {
		r.idle++
		if trigger == "wake" || (r.opts.DecideEvery > 0 && r.idle >= r.opts.DecideEvery) {
			r.idle = 0
			decided = true
			r.opts.Decide(ctx, t)
		}
	}
	r.log.Info("tick", "n", t.N, "trigger", trigger, "reasons", reasons, "decided", decided, "duration", time.Since(start))
}

// supervise keeps one sensor running until ctx is canceled.
func (r *Runtime) supervise(ctx context.Context, s inputs.Sensor) {
	backoff := r.opts.BackoffMin
	for {
		started := time.Now()
		err := s.Run(ctx, r.wake)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Run must not return while ctx is live.
			err = errors.New("Run returned without an error")
		}
		if time.Since(started) >= r.opts.HealthyAfter {
			backoff = r.opts.BackoffMin
		}
		r.log.Error("sensor stopped; restarting", "sensor", s.Name(), "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, r.opts.BackoffMax)
	}
}
