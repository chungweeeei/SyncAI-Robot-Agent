package inputs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
)

func okStatus() RobotStatus {
	return RobotStatus{Mode: "AUTO", Motion: "STAND", Policy: "PPO", PoseValid: true, Map: "dp1f",
		Battery: 80, MotorCount: 12, MotorsSeen: true}
}

func TestTransitions(t *testing.T) {
	cfg := DefaultRobotStateConfig()
	tests := []struct {
		name   string
		mutate func(*RobotStatus)
		want   []string
	}{
		{"no change", func(*RobotStatus) {}, nil},
		{"mode", func(s *RobotStatus) { s.Mode = "MANUAL" }, []string{"mode_changed"}},
		{"estop", func(s *RobotStatus) { s.Motion = "ESTOP" }, []string{"estop"}},
		{"damping", func(s *RobotStatus) { s.Motion = "DAMPING" }, []string{"damping"}},
		{"locomotion is quiet", func(s *RobotStatus) { s.Motion = "LOCOMOTION" }, nil},
		{"pose lost", func(s *RobotStatus) { s.PoseValid = false }, []string{"pose_lost"}},
		{"map", func(s *RobotStatus) { s.Map = "dp2f" }, []string{"map_changed"}},
		{"motors missing", func(s *RobotStatus) {
			s.MotorsSeen, s.Motion, s.Policy = false, "UNKNOWN", "UNKNOWN"
		}, []string{"motion_unknown", "motors_missing"}},
		{"motor fault", func(s *RobotStatus) { s.MotorFaults = []string{"FL_hip"} }, []string{"motor_fault"}},
		{"motor hot", func(s *RobotStatus) { s.HotMotors = []string{"FL_knee"} }, []string{"motor_hot"}},
		{"battery 30", func(s *RobotStatus) { s.Battery = 30 }, []string{"battery_low_30"}},
		{"battery jumps to 9", func(s *RobotStatus) { s.Battery = 9 }, []string{"battery_low_10"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev, cur := okStatus(), okStatus()
			tt.mutate(&cur)
			got, _ := transitions(prev, cur, 0, cfg)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBatteryHysteresis(t *testing.T) {
	cfg := DefaultRobotStateConfig()
	seq := []int{25, 21, 20, 19, 20, 19, 21, 24, 25, 19, 9, 40, 30}
	want := []string{"", "", "battery_low_20", "", "", "", "", "", "", "battery_low_20", "battery_low_10", "", "battery_low_30"}

	prev, notify := okStatus(), batteryLevel(25, cfg.BatteryThresholds)
	prev.Battery = 25
	for i, pct := range seq[1:] {
		cur := okStatus()
		cur.Battery = pct
		var got []string
		got, notify = transitions(prev, cur, notify, cfg)
		if w := want[i+1]; strings.Join(got, ",") != w {
			t.Errorf("step %d (%d%%): got %v, want %q", i+1, pct, got, w)
		}
		prev = cur
	}
}

func TestFromDTODriverSilent(t *testing.T) {
	var d backend.RobotState
	d.LowLevelMode.Policy, d.LowLevelMode.Motion = "PPO", "STAND" // driver 沒回報時的預設值
	st := fromDTO(d, DefaultRobotStateConfig())
	if st.Motion != MotionUnknown || st.MotorsSeen {
		t.Fatalf("driver silent should give UNKNOWN motion, got %+v", st)
	}
	if !strings.Contains(formatRobot(st, DefaultRobotStateConfig()), "motor telemetry missing") {
		t.Error("text should mention missing motor telemetry")
	}
}

func TestFormatRobot(t *testing.T) {
	cfg := DefaultRobotStateConfig()
	st := okStatus()
	st.Motion, st.X, st.Y, st.YawDeg, st.Velocity, st.Battery, st.RSSI = "LOCOMOTION", 3.2, -1, 90, 0.4, 18, -62
	got := formatRobot(st, cfg)
	want := `mode AUTO; motion LOCOMOTION; pose (3.2, -1.0, 90°) on map "dp1f"; moving 0.40 m/s; battery 18% (LOW); wifi -62 dBm; 12 motors ok.`
	if got != want {
		t.Errorf("\ngot  %s\nwant %s", got, want)
	}
}

// fakeBackend 是可以控制 timestamp 和狀態碼的假 backend。
type fakeBackend struct {
	ts     atomic.Int64
	status atomic.Int32
	mode   atomic.Value
}

func newFakeBackend(t *testing.T) (*fakeBackend, *backend.Client) {
	f := &fakeBackend{}
	f.status.Store(http.StatusOK)
	f.mode.Store("AUTO")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := int(f.status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timestamp":          f.ts.Load(),
			"map":                "dp1f",
			"mode":               f.mode.Load(),
			"low_level_mode":     map[string]string{"policy": "PPO", "motion": "STAND"},
			"localization_valid": true,
			"battery_status":     map[string]int{"battery_percentage": 80},
			"motor_status":       []map[string]any{{"name": "FL_hip", "temperature": 40, "error": 0}},
		})
	}))
	t.Cleanup(srv.Close)
	return f, backend.NewClient(srv.URL, time.Second)
}

type wakeLog struct {
	mu      sync.Mutex
	reasons []string
}

func (w *wakeLog) wake(r string) { w.mu.Lock(); w.reasons = append(w.reasons, r); w.mu.Unlock() }
func (w *wakeLog) count(r string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, x := range w.reasons {
		if x == r {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunLifecycle(t *testing.T) {
	f, client := newFakeBackend(t)
	cfg := DefaultRobotStateConfig()
	cfg.PollInterval, cfg.StaleAfter = 10*time.Millisecond, 80*time.Millisecond

	s := NewRobotState(client, cfg)
	w := &wakeLog{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 讓 timestamp 持續前進，模擬 1 Hz 的 node（這裡加速）。
	var frozen atomic.Bool
	go func() {
		for ctx.Err() == nil {
			if !frozen.Load() {
				f.ts.Add(1)
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, w.wake) }()

	// 暖機前：NO DATA，Current 不可用。
	if _, ok := s.Current(); ok {
		t.Error("Current should not be ok before warmup")
	}
	waitFor(t, "online", func() bool { return w.count("robot_state:online") == 1 })
	if _, ok := s.Current(); !ok {
		t.Error("Current should be ok after online")
	}

	// mode 切換會喚醒。
	f.mode.Store("MANUAL")
	waitFor(t, "mode_changed", func() bool { return w.count("robot_state:mode_changed") == 1 })
	f.mode.Store("AUTO")
	waitFor(t, "mode_changed back", func() bool { return w.count("robot_state:mode_changed") == 2 })

	// node 掛掉：backend 一直回 200 但 timestamp 不動 → 過期，而且只喚醒一次。
	frozen.Store(true)
	waitFor(t, "stale", func() bool { return w.count("robot_state:stale") == 1 })
	time.Sleep(3 * cfg.StaleAfter)
	if n := w.count("robot_state:stale"); n != 1 {
		t.Errorf("stale woke %d times, want 1", n)
	}
	if _, ok := s.Current(); ok {
		t.Error("Current should not be ok while stale")
	}
	if got := s.Snapshot().Render(s.Name(), time.Now()); !strings.Contains(got, "STALE") {
		t.Errorf("render = %q, want STALE", got)
	}

	// 404 也不會讓 Run 結束。
	f.status.Store(http.StatusNotFound)
	time.Sleep(5 * cfg.PollInterval)
	f.status.Store(http.StatusOK)

	// 恢復：重新暖機後只發 recovered，不把斷線期間的 mode 變化當成 mode_changed。
	f.mode.Store("MANUAL")
	frozen.Store(false)
	waitFor(t, "recovered", func() bool { return w.count("robot_state:recovered") == 1 })
	if n := w.count("robot_state:mode_changed"); n != 2 { // 斷線前 MANUAL、AUTO 各一次；斷線期間的切換不算
		t.Errorf("mode_changed = %d, want 2", n)
	}

	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

func TestWarmupRejectsFrozenCache(t *testing.T) {
	// node 早就死了，backend 還在回最後一筆快取：timestamp 永遠不前進，所以不能上線。
	f, client := newFakeBackend(t)
	f.ts.Store(100)
	cfg := DefaultRobotStateConfig()
	cfg.PollInterval, cfg.StaleAfter = 10*time.Millisecond, 50*time.Millisecond
	s := NewRobotState(client, cfg)
	w := &wakeLog{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx, w.wake)
	if len(w.reasons) != 0 {
		t.Errorf("unexpected wakes %v", w.reasons)
	}
	if got := s.Snapshot().Render(s.Name(), time.Now()); !strings.Contains(got, "NO DATA") {
		t.Errorf("render = %q, want NO DATA", got)
	}
}
