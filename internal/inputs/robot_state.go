package inputs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
)

// RobotStateSource 是 RobotState 需要的資料來源；*backend.Client 實作了它，測試可以換成假的。
type RobotStateSource interface {
	GetRobotState(ctx context.Context) (backend.RobotState, error)
}

// RobotStateConfig 是 robot_state sensor 的設定。
type RobotStateConfig struct {
	PollInterval time.Duration // 輪詢間隔
	StaleAfter   time.Duration // 超過這麼久沒有新資料，snapshot 就算過期

	// WarmupSamples 是開始回報前，要連續收到幾筆 timestamp 有前進的資料。
	// 用來避開兩件事：robot_state node 剛啟動時 mode 固定先報 AUTO，
	// 以及 node 早就死掉、backend 卻還在回最後一筆快取。
	WarmupSamples int

	MotorHotC int // 馬達溫度 >= 這個值就算過熱。TODO: 確認 G23 的實際門檻

	// BatteryThresholds 是電量往下跨過時要喚醒的門檻，由高到低排列。
	BatteryThresholds []int
	// BatteryHysteresis：電量回升到「已通知的門檻 + 這個值」以上，才重新啟用該門檻。
	BatteryHysteresis int
	BatteryLowPct     int // 文字裡標示 LOW 的電量
	BatteryCritPct    int // 文字裡標示 CRITICAL 的電量
}

// DefaultRobotStateConfig 回傳 Phase 1 的預設值（robot_state 以 1 Hz 發布）。
func DefaultRobotStateConfig() RobotStateConfig {
	return RobotStateConfig{
		PollInterval:      time.Second,
		StaleAfter:        5 * time.Second,
		WarmupSamples:     2,
		MotorHotC:         70,
		BatteryThresholds: []int{30, 20, 10},
		BatteryHysteresis: 5,
		BatteryLowPct:     20,
		BatteryCritPct:    10,
	}
}

// 下面這些字串與 backend 回傳的值一致。
const (
	ModeAuto        = "AUTO"
	MotionEstop     = "ESTOP"
	MotionDamping   = "DAMPING"
	MotionUnknown   = "UNKNOWN"
	PolicyUnknown   = "UNKNOWN"
	policyDefault   = "PPO"
	movingThreshold = 0.05 // m/s，低於這個速度在文字裡就不顯示速度
)

// RobotStatus 是 guard 用來做決策的型別化狀態；LLM 看到的是 formatRobot 產生的文字。
type RobotStatus struct {
	SourceTS    int64  // payload 的 timestamp（秒）
	Mode        string // MAINTENANCE / MANUAL / AUTO / UNKNOWN
	Motion      string // driver 沒回報時強制為 UNKNOWN
	Policy      string // driver 沒回報時強制為 UNKNOWN
	PoseValid   bool
	X, Y        float64
	YawDeg      float64
	Velocity    float64
	Map         string
	Battery     int
	RSSI        int
	MotorCount  int
	MotorsSeen  bool     // false = 沒有任何 motor 資料，driver_manager 可能沒在跑
	MotorFaults []string // error != 0 的關節
	HotMotors   []string // 溫度 >= MotorHotC 的關節
}

// RobotState 是 "robot_state" sensor：輪詢 backend 的 /api/v1/robot/state。
type RobotState struct {
	Latest

	cfg RobotStateConfig
	src RobotStateSource

	// 以下欄位只在 Run 的 goroutine 裡讀寫，不需要加鎖。
	lastTS      int64        // 上一次看到的 payload timestamp
	haveTS      bool         // 是否看過任何 payload
	lastFreshAt time.Time    // 上一次 timestamp 前進的本機時間
	warm        int          // 暖機期間已收到的新資料筆數
	prev        *RobotStatus // 上一次回報的狀態；nil 代表還沒上線過
	stale       bool         // 是否已經發過 stale 的 wake
	battNotify  int          // 已通知過的最低電量門檻；0 代表沒有

	// 給 guard 讀的狀態，Run 寫、其他 goroutine 讀。
	curMu sync.RWMutex
	cur   RobotStatus
	curOK bool
}

// NewRobotState 建立 robot_state sensor。
func NewRobotState(src RobotStateSource, cfg RobotStateConfig) *RobotState {
	return &RobotState{
		Latest: Latest{StaleAfter: cfg.StaleAfter},
		cfg:    cfg,
		src:    src,
	}
}

// Name 實作 Sensor。
func (s *RobotState) Name() string { return "robot_state" }

// Current 回傳給 guard 用的最新狀態。資料還沒暖機完成或已經過期時，ok 為 false，
// guard 應該把 ok == false 當成「擋下所有動作任務」。
func (s *RobotState) Current() (st RobotStatus, ok bool) {
	s.curMu.RLock()
	st, ok = s.cur, s.curOK
	s.curMu.RUnlock()
	if !ok || s.Snapshot().Stale(time.Now()) {
		return RobotStatus{}, false
	}
	return st, true
}

// Run 實作 Sensor。HTTP 錯誤不會讓 Run 結束，只會讓資料自然過期。
func (s *RobotState) Run(ctx context.Context, wake WakeFunc) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		s.poll(ctx, wake)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// poll 執行一次輪詢。
func (s *RobotState) poll(ctx context.Context, wake WakeFunc) {
	now := time.Now()
	dto, err := s.src.GetRobotState(ctx)
	fresh := err == nil && (!s.haveTS || dto.Timestamp != s.lastTS) // 用 != 而不是 >，時鐘往回調也不會卡住

	if !fresh {
		// 請求失敗、404，或 timestamp 沒前進：不呼叫 Set，讓資料自然過期。
		// 過期的那一刻主動喚醒一次，agent 才會立刻知道來源斷了。
		if s.prev != nil && !s.stale && s.Snapshot().Stale(now) {
			s.stale = true
			wake("robot_state:stale")
		}
		return
	}

	// 兩筆新資料之間隔太久（第一次啟動、斷線恢復、node 重啟），就重新暖機。
	if !s.lastFreshAt.IsZero() && now.Sub(s.lastFreshAt) > s.cfg.StaleAfter {
		s.warm = 0
	}
	s.lastTS, s.haveTS, s.lastFreshAt = dto.Timestamp, true, now
	if s.warm < s.cfg.WarmupSamples {
		s.warm++
		if s.warm < s.cfg.WarmupSamples {
			return
		}
	}

	cur := fromDTO(dto, s.cfg)
	s.Set(formatRobot(cur, s.cfg))
	s.curMu.Lock()
	s.cur, s.curOK = cur, true
	s.curMu.Unlock()

	switch {
	case s.prev == nil:
		// 第一次上線：不比較轉變，狀態都在 Text 裡；電量門檻從目前電量開始算，不另外喚醒。
		s.battNotify = batteryLevel(cur.Battery, s.cfg.BatteryThresholds)
		wake("robot_state:online")
	case s.stale:
		// 斷線恢復：斷線前後的差異不逐一喚醒，只發一次 recovered。
		s.battNotify = batteryLevel(cur.Battery, s.cfg.BatteryThresholds)
		s.stale = false
		wake("robot_state:recovered")
	default:
		var reasons []string
		reasons, s.battNotify = transitions(*s.prev, cur, s.battNotify, s.cfg)
		for _, r := range reasons {
			wake("robot_state:" + r)
		}
	}
	s.prev = &cur
}

// fromDTO 把 backend 的 JSON 轉成型別化的狀態。
func fromDTO(d backend.RobotState, cfg RobotStateConfig) RobotStatus {
	st := RobotStatus{
		SourceTS:   d.Timestamp,
		Mode:       d.Mode,
		Motion:     d.LowLevelMode.Motion,
		Policy:     d.LowLevelMode.Policy,
		PoseValid:  d.LocalizationValid,
		Map:        d.Map,
		Battery:    d.BatteryStatus.BatteryPercentage,
		RSSI:       d.NetworkStatus.RSSI,
		MotorCount: len(d.MotorStatus),
		MotorsSeen: len(d.MotorStatus) > 0,
	}
	if st.PoseValid {
		p := d.LocalizationStatus.Position
		st.X, st.Y, st.YawDeg = p.X, p.Y, p.Theta
		st.Velocity = d.LocalizationStatus.Velocity
	}
	// motor_status 為空代表 driver_manager 沒在發布，此時 low_level_mode 的
	// PPO/STAND 是預設值而不是讀值，不能當成 STAND。
	if !st.MotorsSeen {
		st.Motion, st.Policy = MotionUnknown, PolicyUnknown
	}
	for _, m := range d.MotorStatus {
		if m.Error != 0 {
			st.MotorFaults = append(st.MotorFaults, m.Name)
		}
		if m.Temperature >= cfg.MotorHotC {
			st.HotMotors = append(st.HotMotors, m.Name)
		}
	}
	return st
}

// transitions 比較前後兩次狀態，回傳要喚醒的原因，以及更新後的電量通知門檻。
func transitions(prev, cur RobotStatus, battNotify int, cfg RobotStateConfig) ([]string, int) {
	var r []string
	if cur.Mode != prev.Mode {
		r = append(r, "mode_changed")
	}
	if cur.Motion != prev.Motion {
		switch cur.Motion {
		case MotionEstop:
			r = append(r, "estop")
		case MotionDamping:
			r = append(r, "damping")
		case MotionUnknown:
			r = append(r, "motion_unknown")
		}
	}
	if cur.PoseValid != prev.PoseValid {
		if cur.PoseValid {
			r = append(r, "pose_available")
		} else {
			r = append(r, "pose_lost")
		}
	}
	if cur.Map != prev.Map {
		r = append(r, "map_changed")
	}
	if prev.MotorsSeen && !cur.MotorsSeen {
		r = append(r, "motors_missing")
	}
	if hasNew(prev.MotorFaults, cur.MotorFaults) {
		r = append(r, "motor_fault")
	}
	if hasNew(prev.HotMotors, cur.HotMotors) {
		r = append(r, "motor_hot")
	}

	// 電量：先處理回升（hysteresis），再看有沒有往下跨過新的門檻。
	th := cfg.BatteryThresholds
	for battNotify != 0 && cur.Battery >= battNotify+cfg.BatteryHysteresis {
		battNotify = nextHigher(th, battNotify)
	}
	if lvl := batteryLevel(cur.Battery, th); lvl != 0 && (battNotify == 0 || lvl < battNotify) {
		r = append(r, fmt.Sprintf("battery_low_%d", lvl))
		battNotify = lvl
	}
	return r, battNotify
}

// batteryLevel 回傳 pct 已經落到的最低門檻（pct <= 門檻）；高於所有門檻時回傳 0。
func batteryLevel(pct int, thresholds []int) int {
	lvl := 0
	for _, t := range thresholds {
		if pct <= t && (lvl == 0 || t < lvl) {
			lvl = t
		}
	}
	return lvl
}

// nextHigher 回傳比 t 高的下一個門檻；t 已經是最高的就回傳 0。
func nextHigher(thresholds []int, t int) int {
	next := 0
	for _, x := range thresholds {
		if x > t && (next == 0 || x < next) {
			next = x
		}
	}
	return next
}

// hasNew 回報 cur 裡是否有 prev 沒有的元素。
func hasNew(prev, cur []string) bool {
	for _, c := range cur {
		if !slices.Contains(prev, c) {
			return true
		}
	}
	return false
}

// formatRobot 產生給 LLM 的文字。每一種異常都直接寫出「不該做什麼」。
// 這段文字是 prompt 的一部分，所以維持英文。
func formatRobot(st RobotStatus, cfg RobotStateConfig) string {
	var parts []string

	if st.Mode == ModeAuto {
		parts = append(parts, "mode AUTO")
	} else {
		parts = append(parts, fmt.Sprintf("mode %s — agent must not act", st.Mode))
	}

	switch st.Motion {
	case MotionEstop:
		parts = append(parts, "motion ESTOP — robot is emergency-stopped, do not issue any task")
	case MotionDamping:
		parts = append(parts, "motion DAMPING — motors are limp, do not issue any task")
	case MotionUnknown:
		parts = append(parts, "motion UNKNOWN — do not issue motion tasks")
	default:
		m := "motion " + st.Motion
		if st.Policy != policyDefault {
			m += fmt.Sprintf(" (policy %s)", st.Policy)
		}
		parts = append(parts, m)
	}

	if st.PoseValid {
		parts = append(parts, fmt.Sprintf("pose (%.1f, %.1f, %.0f°) on map %q", st.X, st.Y, st.YawDeg, st.Map))
		if st.Velocity >= movingThreshold {
			parts = append(parts, fmt.Sprintf("moving %.2f m/s", st.Velocity))
		}
	} else {
		parts = append(parts, fmt.Sprintf("pose NOT available on map %q — do not issue MOVE tasks", st.Map))
	}

	b := fmt.Sprintf("battery %d%%", st.Battery)
	switch {
	case st.Battery <= cfg.BatteryCritPct:
		b += " (CRITICAL)"
	case st.Battery <= cfg.BatteryLowPct:
		b += " (LOW)"
	}
	parts = append(parts, b)

	if st.RSSI != 0 {
		parts = append(parts, fmt.Sprintf("wifi %d dBm", st.RSSI))
	}

	switch {
	case !st.MotorsSeen:
		parts = append(parts, "motor telemetry missing — driver_manager may be down")
	case len(st.MotorFaults) > 0 || len(st.HotMotors) > 0:
		if len(st.MotorFaults) > 0 {
			parts = append(parts, "motor faults: "+strings.Join(st.MotorFaults, ", "))
		}
		if len(st.HotMotors) > 0 {
			parts = append(parts, fmt.Sprintf("motors hot (>=%d°C): %s", cfg.MotorHotC, strings.Join(st.HotMotors, ", ")))
		}
	default:
		parts = append(parts, fmt.Sprintf("%d motors ok", st.MotorCount))
	}

	return strings.Join(parts, "; ") + "."
}
