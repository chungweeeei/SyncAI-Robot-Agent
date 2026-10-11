# Proposal：借 OM1 的 runtime 設計，在機器狗上跑一個 background agent

> Phase 1 的範圍：只接兩種 sensor 資料（`robot_state`、`task_status`），把它們 fuse 成 prompt，每個 tick 印出來看。
> 不接 LLM、不接 actions。目的是先確認「agent 看到的世界」長什麼樣子，再決定 Phase 2 要怎麼接 LLM。

---

## 1. 背景與目標

我們要在機器狗上跑一個常駐的 agent。它不斷觀察機器人狀態與任務狀態，組成一段 LLM 讀得懂的文字（prompt），之後再由 LLM 決定要做什麼。

OM1（OpenMind 的機器人 agent runtime）已經把這個架構走過一遍，值得借用的是它的 **runtime 骨架**：
sensor 各自在背景更新 → 一個固定節奏的 loop 把所有 sensor 的最新文字收集起來 → fuser 組成 prompt → LLM → actions。

本專案目前已經有第一個 sensor（`robot_state`）和一個很小的 REST client，但還沒有 loop、fuser、config，`cmd/main.go` 還是 hello world。
Phase 1 就是把骨架補齊，讓它能在狗上跑起來、把 prompt 印出來。

---

## 2. OM1 的 runtime 設計重點

`/Users/andy_tseng/Desktop/Projects/OM1` 目前 checkout 的已經是 **Go 版本**（Python 版放在 `python` branch，已退役），所以可以直接參考 Go 的檔案。

### 2.1 Cortex loop（`internal/runtime/runtime.go`）

```go
tickInterval := time.Duration(float64(time.Second) / hertz)
for {
    timer := time.NewTimer(tickInterval)
    select {
    case <-ctx.Done():                 return
    case <-timer.C:                    // 固定節奏
    case <-inputOrchestrator.TickNow(): timer.Stop()   // 某個 sensor 有新資料，提前 tick
    }
    rt.tick(ctx, current, time.Now())
}
```

每個 `tick`：收集所有 sensor 的最新文字 → `fuser.Fuse` → LLM → 執行 tool calls。
重點是 **兩種觸發來源**：固定頻率的 timer，以及 sensor 主動喚醒（`TickNow` 是一個 buffer 為 1 的 channel，多次喚醒會合併）。

### 2.2 Inputs（`internal/inputs/sensor.go`、`orchestrator.go`）

- 每個 sensor 一個 goroutine，各自以自己的節奏取資料並轉成文字。
- Loop 只「讀」sensor 的最新文字，不等它。
- OM1 用的是 **drain-on-read buffer**：讀過就清空，沒新資料就回空字串、prompt 裡不出現。

### 2.3 Fuser（`internal/fuser/fuser.go`）

純粹的字串組裝，版面是：

```
{system_prompt_base}

Governance rules:
{system_governance}

Current time is Monday, January 2, 2006 at 3:04 PM.

Current observations:
- {sensor 1 的文字}
- {sensor 2 的文字}

Available actions:
- move
- speak

{system_prompt_examples}

What will you do next?
```

### 2.4 Config（`config/*.json5`）

`hertz`、`name`、`system_prompt_base`、`system_governance`、`system_prompt_examples`、`agent_inputs`、`cortex_llm`、`agent_actions`。
Sensor 用 `type` 字串對應到 `init()` 時 `inputs.Register(...)` 註冊的 factory。

---

## 3. 本專案要借什麼、不借什麼

| 項目 | 決定 | 原因 |
|---|---|---|
| Tick loop（timer + 喚醒合併） | **借** | 核心骨架 |
| Fuser 版面 | **借**（去掉 KB / memory / MCP，暫無 actions） | 直接對齊 OM1 的 prompt 結構 |
| Config 的三個 prompt 欄位 + hertz | **借**（純 JSON，不用 JSON5） | 零依賴；stdlib `encoding/json` 就夠 |
| Sensor 介面 | **不借，保留本專案的** | 見下方說明 |
| Plugin registry（`Register/Load` + `map[string]any`） | **不借，先明確接線** | 只有兩個 sensor；既有設計偏好型別化的 `XConfig` |
| Mode / hooks / tracer / metrics / zenoh | **不借** | Phase 1 用不到 |

**為什麼保留本專案的 Sensor 介面？**

`internal/inputs/sensor.go` 的設計是 **snapshot + wake**：

```go
type Sensor interface {
    Name() string
    Run(ctx context.Context, wake WakeFunc) error   // 持續更新 snapshot；回 error 由 runtime 以 backoff 重啟
    Snapshot() Snapshot                              // 永遠拿得到最新狀態，不阻塞
}
```

和 OM1 的 drain-on-read 不同，這裡的 snapshot **永遠有值**，而且 `Snapshot.Render` 內建了 `NO DATA yet` / `STALE (last update 7s ago)` 的呈現。
對機器人狀態這種「隨時都該知道」的資料，snapshot 比 buffer 合理：LLM 每次都要看到完整狀態，而不是只看到變化。
`wake` 則對應 OM1 的 `TickNow`，由 sensor 在狀態轉變時主動喚醒（例如 `robot_state:estop`）。

這個介面已經有 `robot_state` 實作與完整測試，沿用它、把 OM1 的 loop 接在上面，成本最低。

---

## 4. `task_status` 的資料來源

已經在 `SyncAI-Robot-Backend` 確認：**任務是 Temporal workflow，沒有 ROS topic**。後端是 FastAPI、port 3000。

| Endpoint | 用途 | 注意事項 |
|---|---|---|
| `GET /api/v1/active_tasks` | 現在在跑的任務清單 | 不會 404（空陣列合法）；後端快取 1.5 s，所以輪詢 2 s；Temporal 掛掉會回 **502**、不會回舊快取；回傳 `as_of`，算「經過多久」要用它、不能用本機時鐘 |
| `GET /api/v1/tasks/{id}` | 單一任務 + 每個 step 的狀態（即時） | 唯一能看到 `PAUSED` 的地方（清單裡暫停中的任務仍顯示 IN_PROGRESS）；已結束的任務在 Temporal retention 內也查得到；step 只有 `id / status / error_msg`，**沒有 type**；workflow query 失敗時 `steps` 會是空陣列；不是本機器人的 id 回 404 |

回應範例：

```json
// GET /api/v1/active_tasks
{
  "tasks": [{
    "id": "robot01-goal-1782786519-3", "run_id": "…", "status": "IN_PROGRESS",
    "started_at": "2026-10-06T02:13:45.123456Z", "source": "DIRECT",
    "schedule_id": null, "kind": "goal", "name": "patrol-A", "map_name": "dp1f"
  }],
  "as_of": "2026-10-06T02:14:30.000000Z"
}

// GET /api/v1/tasks/robot01-goal-1782786519-3
{
  "id": "robot01-goal-1782786519-3", "status": "IN_PROGRESS",
  "steps": [
    {"id": "step1", "status": "COMPLETED",   "error_msg": ""},
    {"id": "step2", "status": "IN_PROGRESS", "error_msg": ""},
    {"id": "step3", "status": "PENDING",     "error_msg": ""}
  ]
}
```

其他要知道的事：

- 任務狀態（wire 上大寫）：`IN_PROGRESS / PAUSED / COMPLETED / FAILED / CANCELED`。`PENDING / PAUSING / CANCELING` 只出現在 POST / DELETE 的 ack，GET 不會回。
- `kind` 是**小寫**：`goal / standup / liedown / task / schedule`；`kind / name / map_name / schedule_id` 可能是 null。
- datetime 全是 tz-aware UTC，序列化成 RFC 3339 帶 `Z`，Go 的 `time.Time` 可以直接 unmarshal。
- 後端一次只允許一個任務（409 busy），但 `active_tasks` 仍可能有 2 筆（排程 + 手動）。
- 取消：被中斷的 step 標 `CANCELED`、`error_msg = "Task canceled"`。失敗：失敗的 step 標 `FAILED` + 真實訊息，後面的 step 留 `PENDING`。

---

## 5. 架構（Phase 1 完成後）

```
cmd/main.go
  flags → config.Load → backend.NewClient
        → inputs.NewRobotState / inputs.NewTaskStatus       （明確接線）
        → fuser.New(PromptConfig)
        → runtime.New(sensors, fuser, log, opts).Run(ctx)

internal/runtime      單一 Timer(1/hertz) | wakeCh → tick；每個 sensor 一個 goroutine，Run 回 error 就 backoff 重啟
internal/fuser        Fuse(now, observations []string) string   ← 純函式，golden test 就是「prompt 長怎樣」
internal/inputs       robot_state（既有）、task_status（新）
internal/backend      robot.go（既有）、task.go（新）
internal/config       JSON config + ${VAR:-default} 展開
config/dog.json       範例設定
```

資料流：

```
backend REST ──2s──▶ task_status ──Set()──▶ Latest ─┐
backend REST ──1s──▶ robot_state ──Set()──▶ Latest ─┤
                           │ wake("robot_state:estop")            │ Snapshot().Render(name, now)
                           ▼                                       ▼
                     runtime loop ──────── tick ──────▶ fuser.Fuse(now, observations) ──▶ stdout / log
                      ▲        ▲
                   timer    wakeCh（合併）
```

---

## 6. 各元件設計

### 6.1 `internal/backend/task.go` — task API 的 DTO 與方法

```go
type ActiveTask struct {
    ID string `json:"id"`; RunID string `json:"run_id"`; Status string `json:"status"`
    StartedAt time.Time `json:"started_at"`; Source string `json:"source"`
    ScheduleID string `json:"schedule_id"`; Kind string `json:"kind"`; Name string `json:"name"`; MapName string `json:"map_name"`
}
type ActiveTasks struct { Tasks []ActiveTask `json:"tasks"`; AsOf time.Time `json:"as_of"` }
type StepState  struct { ID string `json:"id"`; Status string `json:"status"`; ErrorMsg string `json:"error_msg"` }
type TaskState  struct { ID string `json:"id"`; Status string `json:"status"`; Steps []StepState `json:"steps"` }

func (c *Client) GetActiveTasks(ctx context.Context) (ActiveTasks, error)
func (c *Client) GetTaskState(ctx context.Context, id string) (TaskState, error) // "/api/v1/tasks/" + url.PathEscape(id)
```

- JSON null 進 `string` 就是 `""`，不需要指標。
- 註解記錄後端特性：清單快取 1.5 s、GET 即時、PAUSED 只在 GET、404 = 不存在或不是本機、502 = Temporal 掛。

### 6.2 `internal/inputs/task_status.go` — 第二個 sensor

照 `robot_state.go` 的樣板：narrow source interface（測試可換假的）、`Config` + `DefaultConfig()`、嵌入 `Latest`、`Run` 用 ticker 輪詢、`Current()` 給未來的 guard 讀型別化狀態。

```go
type TaskStatusSource interface {
    GetActiveTasks(ctx context.Context) (backend.ActiveTasks, error)
    GetTaskState(ctx context.Context, id string) (backend.TaskState, error)
}
type TaskStatusConfig struct {
    PollInterval       time.Duration // 2s（後端快取 1.5 s）
    StaleAfter         time.Duration // 10s
    FinalLookupRetries int           // 3：任務從清單消失後，查最終狀態的重試次數
    ErrorMsgMaxLen     int           // 160：error_msg 進 prompt 前截斷
}

// guard 用的型別化狀態（LLM 看的是 formatTasks 產生的文字）
type ActiveTaskInfo struct { ID, Kind, Name, Source, Status string; StartedAt time.Time; Age time.Duration; StepIdx, StepCount int; StepID string; StepsKnown bool }
type FinishedTask   struct { ID, Kind, Name, Status string /* "" = 最終狀態未知 */; FinishedAt time.Time; Age time.Duration; StepIdx, StepCount int; StepID, ErrorMsg string; StepsKnown bool }
type TaskSummary    struct { AsOf time.Time; Active []ActiveTaskInfo; Last *FinishedTask }
func (t TaskSummary) Busy() bool

type TaskStatus struct {
    Latest
    cfg TaskStatusConfig; src TaskStatusSource
    // 只在 Run goroutine 讀寫：
    prev  map[string]*taskView   // 上一輪看到的 active 任務（用 map 才能偵測 paused/resumed）
    ended map[string]struct{}    // 已判定結束、但 active_tasks 的快取還列著的 id
    last  *FinishedTask
    online, stale bool
    // guard 讀：
    curMu sync.RWMutex; cur TaskSummary; curOK bool
}
func (s *TaskStatus) Current() (TaskSummary, bool) // 未上線或 stale 時 ok=false；guard 把 !ok 當「不明，擋新任務」
```

**每 2 秒一次的 poll 流程**

1. `GetActiveTasks`。失敗 → 不呼叫 `Set`，讓資料自然過期；第一次過期時 wake `task_status:stale`。
2. 對清單裡每個 id（跳過 `ended` 裡的）：先用清單欄位建 view，再 `GetTaskState(id)` 補 status / steps。
   **若 GET 回來已經是終態 → 立刻 finish、記進 `ended`、不放進 cur。** 原因：清單有 1.5 s 快取，即時 GET 可能比清單先知道任務結束；沒有 `ended` 會變成這輪 `task_completed`、下輪又 `task_started`。
   GET 失敗 → 保留 view、`StepsKnown=false`，整個 poll 不算失敗。
3. `ended` 裡不再被列出的 id 清掉。
4. 上一輪有、這一輪沒有的 id → `GetTaskState(id)` 查最終狀態：
   終態 → finish；非終態 → 留著（visibility 延遲，保守處理）；404 或重試用完 → finish 為「最終狀態未知」；其他錯誤 → 計數 +1、下輪再試。
   這裡要重試的理由：短暫的 Temporal 抖動不能弄丟「FAILED at step 2: nav timeout」，這是這個 sensor 最有價值的輸出。
5. 用 `as_of` 算 `Age`（**不用本機時鐘**）；`Snapshot.Text` 是固定字串，所以「2m ago」在 poll 時就烘進文字。`Set(formatTasks(...))`，發布 summary。
6. 第一次成功 → 只 wake `task_status:online`。斷線恢復 → 只 wake `task_status:recovered`。否則每個轉變各 wake 一次。

**哪些轉變會 wake**

| 事件 | wake reason |
|---|---|
| 新 id 出現 | `task_status:task_started` |
| IN_PROGRESS → PAUSED | `task_status:task_paused` |
| PAUSED → IN_PROGRESS | `task_status:task_resumed` |
| 任務結束 | `task_status:task_completed` / `task_failed` / `task_canceled` / `task_ended`（最終狀態未知） |
| step 前進 | **不 wake**。一次只有一個任務、agent 中途做不了什麼、base tick 會帶到；之後每個 wake 都是一次 LLM 呼叫，要省 |

**prompt 文字**（英文，和 `robot_state` 一樣「每種異常都直接寫出不該做什麼」；label 用 `%q` 的 Name，沒有 Name 就用 ID；忙碌時省略 last task 以省 token）

| 狀態 | 文字 |
|---|---|
| idle，沒有 last | `no active task.` |
| idle，last COMPLETED | `no active task. Last task "patrol-A" (goal) COMPLETED 2m ago.` |
| idle，last FAILED | `no active task. Last task "patrol-A" (goal) FAILED 2m ago at step 2/3 (goto_door): nav timeout.` |
| idle，last FAILED、steps 未知 | `no active task. Last task "patrol-A" (goal) FAILED 2m ago, step detail unavailable.` |
| idle，last CANCELED | `no active task. Last task "patrol-A" (goal) CANCELED 2m ago at step 2/3 (goto_door).` |
| idle，最終狀態未知 | `no active task. Last task "robot01-goal-1782786519-3" ended 2m ago, final status unknown.` |
| running | `task "patrol-A" (goal, started 45s ago) IN_PROGRESS at step 2/3 (goto_door) — robot is busy; do not issue new tasks.` |
| running，kind/name null | `task "robot01-goal-1782786519-3" (started 45s ago) IN_PROGRESS at step 1/1 (step1) — robot is busy; do not issue new tasks.` |
| running，steps 未知 | `task "patrol-A" (goal, started 45s ago) IN_PROGRESS, step detail unavailable — robot is busy; do not issue new tasks.` |
| paused | `task "patrol-A" (goal, started 45s ago) PAUSED at step 2/3 (goto_door) — robot is holding; do not issue new tasks; it must be resumed or canceled first.` |
| 兩個任務 | `2 active tasks: "patrol-A" (goal, scheduled, started 5m ago) IN_PROGRESS at step 3/6 (wp3); "robot01-standup-…" (standup, started 10s ago) IN_PROGRESS at step 1/1 (step1) — robot is busy; do not issue new tasks.` |

### 6.3 `internal/fuser/fuser.go` — prompt 組裝

```go
type PromptConfig struct { SystemPromptBase, SystemGovernance, SystemPromptExamples string }
type Fuser struct { cfg PromptConfig }
func New(cfg PromptConfig) *Fuser
func (f *Fuser) Fuse(now time.Time, observations []string) string
```

跟 OM1 一樣，fuser **只收已經 render 好的字串**，不認識 sensor。這樣它是純函式，golden test 就能直接看到完整 prompt。

版面（空的 section 整段省略、空字串 observation 跳過、**暫時沒有 "Available actions"**）：

```
You are a robot dog.

Governance rules:
Never move when ESTOP.

Current time is Monday, January 2, 2006 at 3:04 PM.

Current observations:
- robot_state: mode AUTO; motion STAND; pose (1.0, 2.0, 90°) on map "dp1f"; battery 80%; 12 motors ok.
- task_status: no active task.

Example: when idle, say hello.

What will you do next?
```

上面這段就是 fuser 的 golden test 期望輸出。真機上 `robot_state` 那行會是 `formatRobot` 的輸出，例如：
`robot_state: mode AUTO; motion LOCOMOTION; pose (3.2, -1.0, 90°) on map "dp1f"; moving 0.40 m/s; battery 18% (LOW); wifi -62 dBm; 12 motors ok.`

來源斷掉時 `Snapshot.Render` 會自動變成：
`robot_state: STALE (last update 7s ago) — do not rely on it.` 或 `task_status: NO DATA yet — do not rely on it.`

### 6.4 `internal/runtime/runtime.go` — tick loop 與 sensor 監督

```go
type PromptFuser interface { Fuse(now time.Time, observations []string) string }
type Tick struct { N uint64; At time.Time; Trigger string /* "timer"|"wake" */; Reasons []string; Prompt string }
type Options struct { Hertz float64; BackoffMin, BackoffMax, HealthyAfter time.Duration; OnTick func(Tick) }
func DefaultOptions() Options  // 0.2 Hz（5 s）、1s、30s、60s、nil

func New(sensors []inputs.Sensor, f PromptFuser, log *slog.Logger, opts Options) *Runtime
func (r *Runtime) Run(ctx context.Context) error  // 正常關閉回 nil，會等所有 sensor goroutine 結束
```

**喚醒合併** — reason 清單和 signal 要在**同一把鎖**下處理。不變式：「signal pending ⇔ reasons pending」。
不這樣做會有兩種 bug：reason 晚一個 base tick 才被記到；或 base tick 之後多一個沒有 reason 的空 tick。

```go
func (r *Runtime) wake(reason string) {            // 滿足 inputs.WakeFunc，永不阻塞
    r.mu.Lock()
    r.reasons = append(r.reasons, reason)
    select { case r.wakeCh <- struct{}{}: default: }
    r.mu.Unlock()
}
func (r *Runtime) takeReasons() []string {
    r.mu.Lock(); defer r.mu.Unlock()
    rs := r.reasons; r.reasons = nil
    select { case <-r.wakeCh: default: }           // 吃掉屬於這批 reasons 的 signal
    return rs
}
```

**主迴圈** — 單一 `time.Timer`、每個 tick 之後 `Reset`，不用 `Ticker`（否則 wake tick 後 10 ms 又來一個 base tick）。語意是「base rate 是最長的空閒間隔」。go.mod 是 1.24，`Reset` 不需要 drain。

```go
timer := time.NewTimer(interval); defer timer.Stop()
for {
    trigger := "timer"
    select {
    case <-ctx.Done(): wg.Wait(); return nil
    case <-timer.C:
    case <-r.wakeCh: trigger = "wake"
    }
    r.tick(ctx, trigger, r.takeReasons())
    timer.Reset(interval)
}
```

tick 在單一 goroutine 跑、永不重疊；tick 期間進來的多個 wake 合併成恰好一個後續 tick。

**Sensor 監督** — 每個 sensor 一個 goroutine：

- `Run` 回來後 `ctx.Err() != nil` → 離開，不重啟。
- 回 `nil` 但 ctx 沒結束 → 違反契約，當 error 重啟。
- **只有這次跑超過 `HealthyAfter`（60 s）才重置 backoff**，否則每次秒掛的 sensor 永遠 1 s 重啟一次。
- backoff 等待要 select ctx；`backoff = min(backoff*2, BackoffMax)`。
- Phase 1 不 recover panic，讓它大聲掛。

**其他**

- 啟動時不立刻 tick：sensor 的 `*:online` wake 會帶出第一個 tick。若 base tick 先到，prompt 會出現 `NO DATA yet`，這是誠實的。
- 所有 sensor 用同一個 `now` render；順序 = sensors slice 順序。
- 多行 prompt 不塞進 slog：runtime 透過 `opts.OnTick(Tick)` 交出去，main 決定怎麼印；slog 每個 tick 一行（n、trigger、reasons、duration）。

### 6.5 `internal/config/config.go` + `config/dog.json`

```go
type Config struct {
    Name string `json:"name"`; Hertz float64 `json:"hertz"`; BackendURL string `json:"backend_url"`
    SystemPromptBase string `json:"system_prompt_base"`; SystemGovernance string `json:"system_governance"`; SystemPromptExamples string `json:"system_prompt_examples"`
}
func Load(path string) (Config, error) // ReadFile → expandEnv → json.Decoder{DisallowUnknownFields} → Validate
func (c Config) Validate() error        // name 非空；0 < hertz <= 10；URL 是 http/https 且有 host；system_prompt_base 非空
func expandEnv(s string) string         // regexp `\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`
```

- `${VAR:-default}` 的展開用 regexp 自己做（和 OM1 一樣），不用 `os.Expand`，因為後者會把 prompt 文字裡的 `$5`、`$x` 也改掉。
- config 套件不 import 任何 internal；main 把 `Config` 映射成 `fuser.PromptConfig`。

```json
{
  "name": "dog",
  "hertz": 0.2,
  "backend_url": "${SYNCAI_BACKEND_URL:-http://localhost:3000}",
  "system_prompt_base": "You are the on-board agent of a quadruped robot dog. …",
  "system_governance": "Never issue a task while the robot is emergency-stopped. …",
  "system_prompt_examples": ""
}
```

`hertz: 0.2` 代表每 5 秒一個 base tick。之後接 LLM 時，每個 tick 都是一次呼叫，所以 base rate 刻意放低，靠 sensor 的 wake 處理需要即時反應的事件。

### 6.6 `cmd/main.go`、`Makefile`

- flags：`-config config/dog.json`、`-log-level info`、`-print-prompt`（Phase 1 預設 true）。
- `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`；slog TextHandler 到 stderr。
- 一個 `backend.NewClient(cfg.BackendURL, time.Second)` 共用。
- 明確接線：`NewRobotState(client, DefaultRobotStateConfig())`、`NewTaskStatus(client, DefaultTaskStatusConfig())`。
- `OnTick`：往 stdout 印 `===== tick 12 [wake] [task_status:task_started] =====` 再接 prompt。
- Makefile：`run: go run ./cmd -config config/dog.json`；新增 `test-race`。

### 6.7（可選）啟動時補上「上一個任務」

`GET /api/v1/task_history?page_size=1` + `GetTaskState` 拿 step 細節，在 `TaskStatus.Run` 開頭做一次。
只有想讓 agent 重啟後第一個 prompt 就知道「狗為什麼在這裡」才需要，Phase 1 驗收不需要。

---

## 7. 測試

**task_status**（`httptest` 假後端，和 `robot_state_test.go` 的 `fakeBackend` / `wakeLog` / `waitFor` 同一套路；`as_of` 固定成離本機時鐘很遠的時間，`Age` 的斷言就能證明沒用本機時鐘）

- 任務出現 → 恰好一個 `task_started`；IN_PROGRESS → PAUSED → IN_PROGRESS → `task_paused`、`task_resumed`。
- 只有 step 前進 → 不 wake，文字下輪更新。
- 消失且 COMPLETED / FAILED（error_msg 含換行且超長 → 被 sanitize）/ CANCELED → 正確 reason + last-task 文字。
- 消失、`tasks/{id}` 404 → `task_ended`；502 兩次後 FAILED → 單一 `task_failed`；502 三次 → `task_ended`。
- 即時 GET 已終態但清單還列著 → 一個 `task_completed`，下輪不會 `task_started`。
- `steps` 空陣列 → "step detail unavailable"；單一任務 GET 失敗但清單 OK → 任務保留。
- 兩個任務 → 依 `started_at` 排序、都印、`len(Current().Active)==2`。
- `active_tasks` 502 → 不 `Set`、一個 `stale` wake、Render 含 STALE；恢復 → 只 `recovered`。
- 純函式 table test：`diff`、`formatTasks`（上表每一列）、`currentStep`、`finalStep`、`fmtAge`。

**fuser**：golden（§6.3）；省略 section；config 字串前後空白；nil observations。

**runtime**（假 sensor：計 Run 次數、前 N 次失敗、阻塞到 ctx 結束、暴露拿到的 `wake`）

- Hertz=100 → 100 ms 內 ≥5 個 tick、trigger `timer`。
- 合併：`OnTick` 阻塞 tick #1，期間 20 個 wake → 恰好一個後續 tick 帶全部 20 個 reason。
- 單一 wake → 恰好一個 tick（證明不變式）。
- sensor 失敗兩次 → Run ≥3 次；回 `nil` 也重啟；backoff 遞增。
- cancel → 1 s 內回 nil、goroutine 全部結束；關閉後再 wake 不 panic。

**config**：env 覆蓋 / 空 env 落到 default / 未知 key 拒絕 / hertz 0、11 拒絕 / 壞 URL 拒絕 / prompt 裡的 `$5` 不動。

---

## 8. 驗證步驟

1. `make test`、`go test -race ./...`、`make vet` 全綠。
2. **離線看 prompt**：`go test ./internal/fuser -run TestFuse -v`，golden 就是完整 prompt。
3. **沒後端也能跑**：`go run ./cmd -config config/dog.json`（後端連不上）→ 每 5 s 一個 tick，prompt 裡是 `NO DATA yet` 行；Ctrl-C 乾淨退出。
4. **接真後端**：`SYNCAI_BACKEND_URL=http://<dog>:3000 go run ./cmd -config config/dog.json`
   - 2 s 內看到 `robot_state:online`、`task_status:online` 的 wake tick，兩行 observation 都有真資料。
   - 從 console 丟一個任務 → `task_started`、prompt 變 busy 句；pause → `task_paused`；resume → `task_resumed`；cancel → `task_canceled`，prompt 變回 `no active task. Last task … CANCELED …`。
   - 關掉後端 → 10 s 內 `task_status:stale`；開回來 → `recovered`。
5. 看 prompt 內容是否合理，決定 Phase 2 的措辭調整。

---

## 9. 之後的路線（不在這次範圍）

- **Phase 2：接 LLM。** 在 `tick` 裡 `fuser.Fuse` 之後呼叫 LLM；fuser 加 "Available actions" section；`-print-prompt` 改預設 false。
- **Phase 3：actions + guard。** 把 LLM 輸出對應到後端的 `POST /api/v1/tasks`（step type `MOVE / STANDUP / LIEDOWN / SPEAK / WAIT`）；guard 用 `RobotState.Current()` 和 `TaskStatus.Current()` 的型別化狀態擋動作（ESTOP、pose 無效、busy），**不解析 prompt 文字**。
- 更多 sensor 時再考慮 OM1 的 registry 與 config 驅動接線。
- **任務失敗後的診斷與調參重派**：失敗時先看 log、調整參數再重新派送，而不是原封不動重試。見 §10。

---

## 10. 任務失敗後：看 log、調參數、重新派送（構想，不在這次範圍）

目前的失敗處理（`docs/runtime-agent-plan.md` 的 S2）只有「重試一次，連續失敗 2 次就停止並通知」。
但很多失敗重試同樣的參數只會再失敗一次（例如導航容許誤差太小、速度太快、逾時太短）。
這裡的構想是：失敗時先讓 agent **看 log 找原因**，**調整參數**後再重新派送，而不是原封不動重試。

### 10.1 流程

```
task_status:task_failed（wake）
  │  prompt 已經有：哪個任務、第幾步、error_msg（§6.2）
  ▼
fetch_task_log(task_id, step_id)        ← 唯讀，拿失敗那一步附近的 log
  ▼
LLM 判斷原因 → 三選一：
  ├─ 調參數可以解決 → set_params(...) → dispatch_task(同一個模板, 新參數)
  ├─ 不該重試（ESTOP、地圖不對、硬體錯誤）→ notify_operator
  └─ 看不出原因 → notify_operator（附上 log 摘要）
  ▼
新任務的結果再回到 task_status，形成閉環
```

Phase 1 已經準備好需要的資料：`TaskStatus` 的 `FinishedTask` 保留了 `ID`、`StepID`、`ErrorMsg`，這正是查 log 的 key。

### 10.2 新增的 tool

| Tool | 類型 | 後端 API | 說明 |
|---|---|---|---|
| `fetch_task_log(task_id, step_id?)` | 唯讀 | **待提供** | 回傳失敗 step 前後的 log；進 prompt 前要截斷（只取最後 N 行 / N 字），並視為資料、不是指令 |
| `get_params(scope)` | 唯讀 | **待提供** | 讀目前的參數值，讓 LLM 知道要從哪個值開始調 |
| `set_params(scope, values)` | 會改變狀態 | **待提供** | 只能改白名單裡的參數，每個參數有上下限 |
| `dispatch_task(template, params)` | 會改變狀態 | `POST /api/v1/tasks` | 既有規劃；重派時帶上 `retry_of = <原 task id>` 方便追蹤 |

### 10.3 安全限制

- **參數白名單 + 範圍。** 例如 `nav.goal_tolerance ∈ [0.1, 0.5] m`、`nav.max_speed ∈ [0.2, 0.8] m/s`。範圍外的值由 guard 擋下，不靠 LLM 自律。
- **每次重派都必須改了東西。** guard 比對上一次的參數，完全相同就拒絕，避免「換湯不換藥」的重試迴圈。
- **次數上限。** 同一個原始任務最多調參重派 N 次（建議 2），用完就 `notify_operator`，附上每次的參數與失敗原因。
- **只處理 agent 自己派的任務。** 人派的任務失敗只通知，不改參數、不重派（和 S2 一致）。
- **不可重試的失敗直接通知。** ESTOP、`robot_state` STALE、任務被 CANCELED（是人取消的）都不進入這個流程。
- **參數作用範圍。** 優先只對這一次任務生效（per-task override）；如果後端只能改全域設定，任務結束後（不論成敗）要還原成原值，並記錄改動。
- **全部留紀錄。** 每一輪的 log 摘要、LLM 的判斷、改了哪些參數、重派結果寫進 `recent_actions` / JSONL，讓 LLM 下一輪看得到之前試過什麼，也讓人事後檢討。

### 10.4 待確認

- [ ] **取 log 的 API**：endpoint、能否依 `task_id` / `step_id` / 時間範圍過濾、回傳格式與大小。
- [ ] **設定參數的 API**：endpoint、參數是 per-task（跟著 `POST /tasks` 帶）還是全域設定、改了之後何時生效。
- [ ] **可調參數清單**：哪些參數開放給 agent、各自的安全範圍與預設值。
- [ ] **錯誤分類**：哪些 `error_msg` / log 特徵代表「調參可解」、哪些代表「不該重試」——可以先整理成 prompt 裡的 examples。
