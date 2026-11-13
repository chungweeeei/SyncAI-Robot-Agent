# 在 AGX Orin 32GB（JetPack 6）上實測 local LLM

Mac simulation 階段通過後才做。目的是量**效能**：延遲、tok/s、記憶體、功耗。

> 狀態：這份文件整理自 2026-11-13 的網路查證，**還沒有在 Orin 上實際跑過**。
> 標示「未證實」的項目請在實機上確認。

## 1. 編譯 agent

```bash
make build-arm64     # 產出 bin/app-linux-arm64 與 bin/bench-linux-arm64
```

只用 stdlib，沒有 cgo，直接把 binary 與 `config/` 複製到 Orin 即可。

## 2. llama-server（JetPack 6 / L4T r36.x / CUDA 12.6）

- 在 Orin 上對 CUDA 12.6 自行 build，或釘住 `r36.4` 的 image tag
  （例如 `ghcr.io/nvidia-ai-iot/llama_cpp:b9066-r36.4.tegra-aarch64-cu129-22.04`）。
- **不要用 `:latest-jetson-orin`**：這個 tag 看起來已經指向 JetPack 7（r38 / CUDA 13）的 image。
- 量化用 Q4_K_M。SM 8.7 上 FP8 會輸出亂碼，不要用。
- MoE 模型在 r36.4.7 上有 decode 卡住的問題，workaround 是設 `CUDA_SCALE_LAUNCH_QUEUES=1x`。
  NVIDIA 說會在之後的 JetPack 修掉，是哪一版未證實。
- 固定 context 長度（先用 8192），開機後先送一個請求 warm up，避免第一次決策遇到冷啟動。
- 備案是 Ollama：官方有 `ollama-linux-arm64-jetpack6` 版本；在 Docker 裡要設 `JETSON_JETPACK=6`。

## 3. 量測

用 Mac 階段留下的同一批記錄，條件才一致：

```bash
SYNCAI_LLM_MODEL=<名稱> ./bench-linux-arm64 -config config/dog.json \
  -cases config/bench_cases.json -log shadow.jsonl -repeat 3
```

同時另開一個終端機記錄記憶體與功耗：

```bash
tegrastats --interval 1000 --logfile tegrastats-<名稱>.log
```

量測條件：

- **navigation stack 要在跑**。Jetson 的 CPU 與 GPU 共用記憶體，LLM 的權重與 KV cache 會直接和 Point-LIO、planner 搶。
- 用**實際部署的 power mode**（`nvpmodel -q` 確認），不要用 MAXN。
- 每個模型至少連續跑幾分鐘，才看得到溫度造成的降頻。

## 4. 候選模型（GGUF Q4_K_M）

| 模型 | 授權 | 為什麼測它 | 已知的 Orin 數字 |
|---|---|---|---|
| Qwen3.5-4B | Apache-2.0 | 4B 級 tool calling 分數最高（BFCL-v3 約 70.6） | AGX Orin 32GB、JP 6.2.1、llama.cpp：約 25.6 tok/s（第三方量測） |
| Nemotron-3.5-Bolt-E4B | OpenMDW-1.1 | 最不容易亂編 tool 或參數（RelyToolBench 63–69） | AGX Orin 64GB、llama.cpp：約 20 tok/s，JetPack 版本未標示；JP6 相容性未證實 |
| Gemma 4 E2B | Apache-2.0 | 省記憶體、速度快 | AGX Orin 64GB、llama.cpp：約 38 tok/s，JetPack 版本未標示 |
| Qwen3.5-9B | Apache-2.0 | 品質上限對照 | AGX Orin 64GB、vLLM：約 16.8 tok/s，JetPack 版本未標示 |

- 表中除了第一列，其餘數字來自 Jetson AI Lab 的 benchmark，量測機器是 AGX Orin **64GB**，而且沒有標示 JetPack 版本，只能當參考。
- 各模型在 JP6 上的實際記憶體用量、首字延遲、冷啟動時間，公開資料幾乎沒有，要自己量。

## 5. 要記錄的結果

| 模型 | 格式合法率 | guard 通過率 | 預期 tool 一致率 | 延遲 p50 / p95 | tok/s | 記憶體峰值 | 功耗 |
|---|---|---|---|---|---|---|---|
| | | | | | | | |
