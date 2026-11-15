# 在 Mac 的 simulation 上跑 Shadow mode

agent 在 Mac 上看到的是和實機同一套 backend API（`:3000`），所以這裡驗證的是**行為**：
模型在各種事件下會提出什麼 tool call、guard 怎麼判定。速度與記憶體要到 Orin 才量（`docs/orin-llm.md`）。

> 狀態：agent 這一側（本 repo）已用假的 backend 與假的模型跑過端到端。
> 下面「模擬環境」與「local LLM」兩節的指令**還沒有在這台 Mac 上實際跑過**，請以各 repo 的 README 為準。

## 1. Local LLM

建議用 `llama-server`，和之後 Orin 上用同一個 server、同一份 GGUF 檔。

```bash
brew install llama.cpp
# 以 Qwen3.5-4B Q4_K_M 為例；GGUF 檔請自行下載
llama-server -m ~/models/<model>.gguf --port 8080 --ctx-size 8192 --jinja
```

- `--jinja` 啟用模型自己的 chat template，tool call 才會被解析成 `tool_calls`。
- 一個 `llama-server` 只載入一個模型；換模型就重啟 server。
- 推理型模型（例如 Qwen3.5）預設會先「思考」再回答。`config/dog.json` 的 `llm.thinking` 預設為 `false`，
  agent 會在請求裡帶 `chat_template_kwargs: {"enable_thinking": false}` 把它關掉。
  在這台 Mac 上以 Qwen3.5-4B Q4_K_M 實測：開著思考時 13 題只有 5 題下出合法的 tool call
  （思考把 512 個 token 用完，每題約 10 秒）；關掉後 13 題全部合法，每題約 1.6 秒。
  要開思考的話，`max_tokens` 必須一起調高。
- 備案是 Ollama（這台 Mac 已安裝，但目前沒有任何模型）：
  `SYNCAI_LLM_URL=http://localhost:11434 SYNCAI_LLM_MODEL=<ollama 的模型名稱>`。

## 2. 模擬環境

三個 repo 都不需要修改。

| Repo | 角色 | 啟動方式 |
|---|---|---|
| `SyncAI-Robot-Simulation` | MuJoCo 模擬的 G23，扮演 gait controller 與 Livox lidar | README 的 Setup，以及「The sim as the robot's lidar」一節 |
| `SyncAI-Robot-Workspace` | navigation stack、postgres、temporal | 該 repo 的 README |
| `SyncAI-Robot-Backend` | `:3000` 的 REST API | 該 repo 的 README |

Simulation 在 Mac（OrbStack）上要注意的一點，節錄自它的 README：lidar 與 driver 的兩個 IP 在 Mac 上不存在，
要先加到 VM 的 loopback，再重啟 robot container。

```bash
docker run --rm --net host --privileged alpine ip addr add 192.168.1.103/32 dev lo
docker run --rm --net host --privileged alpine ip addr add 192.168.1.172/32 dev lo
docker restart robot01
SIM_ARGS="--teleop -p lidar_link:=livox" docker compose up -d
```

確認 backend 起來了：

```bash
curl -s http://localhost:3000/health
curl -s http://localhost:3000/api/v1/robot/state
```

## 3. 跑 agent

```bash
make run            # shadow mode：每個事件問一次模型，決策寫到 shadow.jsonl
make run-prompt     # 不接模型，只印出每個 tick 的 prompt
```

常用參數與環境變數：

| 項目 | 預設 | 說明 |
|---|---|---|
| `SYNCAI_BACKEND_URL` | `http://localhost:3000` | backend |
| `SYNCAI_LLM_URL` | `http://localhost:8080` | OpenAI 相容端點 |
| `SYNCAI_LLM_MODEL` | `local` | 模型名稱（llama-server 會忽略，Ollama 需要） |
| `-shadow-log` | `shadow.jsonl` | 決策記錄；`""` 表示不寫檔 |
| `-print-prompt` | false | 同時把 prompt 印到 stdout |
| `-no-llm` | false | 不呼叫模型 |

stderr 每個決策會有一行，例如：

```
level=INFO msg="shadow decision" tick=3 tool=create_task arguments="{…}" allowed=false refusal="a task is already active"
```

## 4. 測試劇本

任務由你從前端或 `curl` 下，agent 只旁觀。每個劇本做完後看 `shadow.jsonl` 對應的幾行。

| # | 操作 | 預期的 wake | 要看什麼 |
|---|---|---|---|
| 1 | 正常巡邏一趟 | `task_started`、`task_completed` | 模型是否 `do_nothing` 或給出合理的 `report`；有沒有在任務進行中亂下 `create_task`（會被 guard 拒絕） |
| 2 | 把目標設在 keepout 或被佔用的位置 | `task_failed` | `report` 是否正確解讀錯誤訊息並提出建議 |
| 3 | 任務中途暫停、恢復、取消 | `task_paused`、`task_resumed`、`task_canceled` | 模型對暫停狀態的理解 |
| 4 | `POST /api/v1/robot/estop {"locked": true}` | `robot_state:safety_locked`、`task_canceled` | 所有 `create_task` 都被 guard 拒絕 |
| 5 | 停掉 `robot_state` node 或 backend | `robot_state:stale` / `task_status:stale` | 模型是否停止依賴過期資料 |

驗收：

- backend 的任務歷史裡**沒有任何 agent 建立的任務**（agent 只有 GET）。
- 每個事件在 `shadow.jsonl` 都有一行，且 `allowed` / `refusal` 合理。

## 5. 比較模型

每換一個模型（重啟 `llama-server`），跑一次：

```bash
SYNCAI_LLM_MODEL=<名稱> go run ./cmd/bench -config config/dog.json \
  -cases config/bench_cases.json -log shadow.jsonl
```

- `-cases`：13 個手寫題目，每題有預期的 tool。
- `-log`：重放 shadow 記錄裡的真實 prompt（沒有預期答案，只看格式合法率與 guard 通過率）。
- `-repeat 3`：每題問三次，看穩定度。
- `-v`：印出每一題的回答，不只印出沒過的。

輸出範例：

```
model <名稱> at http://localhost:8080
  asked              17 (0 failed to complete)
  well-formed calls  17/17 (100%)
  passed the guard   16/17 (94%)
  expected tool      11/13 (85%)
  latency            p50 900 ms, p95 2100 ms
  decode rate        61.3 tok/s (mean)
```

（上面的數字只是格式示意，不是量測結果。）
