// agmem HTTP client（agmem push/status/kb 用；ask/clear 走本地 agmemd）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// agmemClient 简单 REST client（Bearer 机器 token）。
type agmemClient struct {
	base  string
	token string
	http  *http.Client
}

func newAgmemClient() (*agmemClient, error) {
	base := os.Getenv("AGMEM_URL")
	if base == "" {
		return nil, fmt.Errorf("AGMEM_URL 未设置（例 https://agmem-test.example.com）")
	}
	token := os.Getenv("AGMEM_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("AGMEM_TOKEN 未设置（找管理员申请机器 token）")
	}
	return &agmemClient{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *agmemClient) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agmem 不可达: %w（检查 AGMEM_URL=%s 与服务端网络连通性）", err, c.base)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// 401 单独给出可行动文案（UC-001 B1 / UC-009 B1）：最常见根因是
		// AGMEM_TOKEN 过期/串了环境，或 AGMEM_URL 指向了别的环境。
		detail := truncateBytes(data)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			detail = e.Error
		}
		return fmt.Errorf("agmem [401]: %s（检查 AGMEM_URL/AGMEM_TOKEN，凭据见 ~/.config/agmem/agmem.env）", detail)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("agmem [%d]: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("agmem [%d]: %s", resp.StatusCode, truncateBytes(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// —— API 返回结构（与 agmem internal/api 契约一致）——

type apiTaskItem struct {
	ID           int64   `json:"id"`
	Status       string  `json:"status"`
	OriginalText string  `json:"original_text"`
	CleanText    *string `json:"clean_text"`
	Error        *string `json:"error"`
}

type apiTask struct {
	ID     int64         `json:"id"`
	Status string        `json:"status"`
	Items  []apiTaskItem `json:"items"`
}

type apiKB struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	OwnerSub   string `json:"owner_sub"`
	DirCount   int64  `json:"dir_count"`
	ItemCount  int64  `json:"item_count"`
	Version    uint64 `json:"version"`
	LastTaskID int64  `json:"last_task_id"`
	LastTaskSt string `json:"last_task_status,omitempty"`
	Role       string `json:"role,omitempty"`
}

type apiDir struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
