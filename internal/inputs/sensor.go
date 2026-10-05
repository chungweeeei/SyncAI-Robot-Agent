// Package inputs 定義 agent 如何觀察機器人與周遭環境。
package inputs

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// WakeFunc 請 runtime 盡快執行一次 tick。
// reason 會寫進 trace（例如 "robot_state:estop"、"task_status:task_failed"）。
// 呼叫它永遠不會阻塞，可以從任何 goroutine 呼叫；
// tick 執行期間進來的多次喚醒，會合併成一次。
type WakeFunc func(reason string)

// Sensor 負責觀察機器人或環境的某一個面向，
// 並把最新狀態整理成一小段 LLM 讀得懂的文字。
//
// 推送型來源（ROS topic、WebSocket）和輪詢型來源（REST）從外面看起來一樣：
// 都是一個 Run 迴圈，持續讓 snapshot 保持在最新狀態。
// guard 需要檢查的 sensor，會在自己的具體型別上另外提供型別化的 getter；
// guard 絕對不可以去解析 Snapshot 的文字。
type Sensor interface {
	// Name 是這個 sensor 在 prompt 和 trace 裡的識別名稱，例如 "robot_state"。
	Name() string

	// Run 持續更新 snapshot，直到 ctx 被取消為止。
	// 回傳 error 時，runtime 會以 backoff 的方式重新啟動它。
	Run(ctx context.Context, wake WakeFunc) error

	// Snapshot 回傳最新狀態。它必須是並發安全的，
	// 並且不能阻塞。
	Snapshot() Snapshot
}

// Snapshot 是 sensor 回報的最新狀態。
type Snapshot struct {
	// Text 是用於 prompt 的一到兩行文字；空字串表示沒有要回報的內容。
	// 欄位層級的警告（例如 "driver not responding"）也放在這裡。
	Text string

	// UpdatedAt 是這個 process 最後一次收到來源更新的時間，而不是來源自己的時間戳
	// （例如 RobotState 的時間戳是另一個時鐘的整秒）。
	UpdatedAt time.Time

	// StaleAfter 是來源可以保持沉默的最長時間，超過這個時間整個 snapshot 就不可靠。
	// 設為零表示永遠不會過期。
	StaleAfter time.Duration
}

func (s Snapshot) Stale(now time.Time) bool {
	if s.StaleAfter == 0 {
		return false
	}
	return s.UpdatedAt.IsZero() || now.Sub(s.UpdatedAt) > s.StaleAfter
}

// Render 將 snapshot 格式化成以 sensor 名稱為前綴的 prompt 行。
// 當沒有要回報的內容時，回傳 ""。
func (s Snapshot) Render(name string, now time.Time) string {
	switch {
	case s.Stale(now) && s.UpdatedAt.IsZero():
		return fmt.Sprintf("%s: NO DATA yet — do not rely on it.", name)
	case s.Stale(now):
		age := now.Sub(s.UpdatedAt).Round(time.Second)
		return fmt.Sprintf("%s: STALE (last update %s ago) — do not rely on it.", name, age)
	case s.Text == "":
		return ""
	default:
		return name + ": " + s.Text
	}
}

// Latest 是一個並發安全的 sensor snapshot 容器。
// 將它嵌入到 sensor 中以取得 Snapshot()；在 Run 中呼叫 Set。
type Latest struct {
	StaleAfter time.Duration // set once before Run starts

	mu   sync.RWMutex
	text string
	at   time.Time
}

// Set 記錄新的狀態文字，並將來源的最後更新時間設為現在。
func (l *Latest) Set(text string) {
	l.mu.Lock()
	l.text, l.at = text, time.Now()
	l.mu.Unlock()
}

// Snapshot 回傳最新狀態。
func (l *Latest) Snapshot() Snapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return Snapshot{Text: l.text, UpdatedAt: l.at, StaleAfter: l.StaleAfter}
}
