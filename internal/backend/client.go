package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound 表示 backend 回了 404。
// 對 /api/v1/robot/state 來說，代表 robot_state node 從沒發布過，或 backend 剛重啟、快取是空的。
var ErrNotFound = errors.New("backend: not found")

// Client 呼叫 backend 的 REST API。 可以被多個 goroutine 同時使用。
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient 建立 client。baseURL 例如 "http://localhost:3000"；
// timeout 是單一請求的上限，應該比 sensor 的輪詢間隔短。
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: timeout,
		},
	}
}

// getJSON 送出 GET 請求，並把 200 的回應解碼到 out。
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("GET %s: %w", path, ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("GET %s: unexpected status %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}
