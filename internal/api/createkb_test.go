package api

// G20：POST /api/kb 建库一致性——先 EnsureCollection 再落 MySQL kb 行。
// EnsureCollection 失败必须 5xx 且不创建 kb 记录（避免「有 kb 无 collection」
// 的脏状态）；成功/重复路径语义不变。
//
// store 用 fake kbCreator 注入（免 MySQL），qdrant 用 httptest fake。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sre/agmem/internal/auth"
	"sre/agmem/internal/config"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
)

type fakeKBCreator struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeKBCreator) CreateKB(_ context.Context, name, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.err != nil {
		return 0, f.err
	}
	return 99, nil
}

func (f *fakeKBCreator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newCreateKBTestServer(t *testing.T, qh http.Handler, creator kbCreator) *httptest.Server {
	t.Helper()
	qsrv := httptest.NewServer(qh)
	t.Cleanup(qsrv.Close)
	s := &Server{
		Machine:   auth.NewMachineAuth("test-token"),
		Session:   auth.NewSession(time.Hour),
		Cfg:       config.Config{KB: "sre", QdrantDim: 1024},
		Qdrant:    qdrant.New(qsrv.URL, "", 2*time.Second),
		KBCreator: creator,
	}
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func postCreateKB(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/api/kb", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// qdrantCreateFake：GET /collections/{n} 返回 getStatus，PUT /collections/{n} 返回 200。
func qdrantCreateFake(getStatus int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(getStatus)
			if getStatus == http.StatusOK {
				_, _ = w.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"bm25":{}}}}}}`))
			} else {
				_, _ = w.Write([]byte(`{"status":{"error":"boom"}}`))
			}
			return
		}
		_, _ = w.Write([]byte(`{"result":true}`))
	})
}

// EnsureCollection 失败 → 502 且绝不落 kb 行（G20 核心）。
func TestCreateKB_EnsureCollectionFailureNoKBRow(t *testing.T) {
	creator := &fakeKBCreator{}
	ts := newCreateKBTestServer(t, qdrantCreateFake(http.StatusInternalServerError), creator)

	code, body := postCreateKB(t, ts.URL, `{"name":"newkb"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", code, body)
	}
	if creator.callCount() != 0 {
		t.Fatalf("EnsureCollection failed → must NOT create kb row, got calls=%v", creator.calls)
	}
	if !strings.Contains(body, "collection") {
		t.Fatalf("error must mention collection, got %s", body)
	}
}

// collection 新建成功 → 201 且 kb 行创建（EnsureCollection 在 CreateKB 之前）。
func TestCreateKB_EnsureCollectionThenCreate(t *testing.T) {
	creator := &fakeKBCreator{}
	ts := newCreateKBTestServer(t, qdrantCreateFake(http.StatusNotFound), creator)

	code, body := postCreateKB(t, ts.URL, `{"name":"newkb"}`)
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", code, body)
	}
	if creator.callCount() != 1 || creator.calls[0] != "newkb" {
		t.Fatalf("kb row must be created once, got %v", creator.calls)
	}
	if !strings.Contains(body, `"id":99`) || !strings.Contains(body, `"name":"newkb"`) {
		t.Fatalf("body wrong: %s", body)
	}
}

// 已存在 kb → 409（collection 已幂等确保存在，不影响既有语义）。
func TestCreateKB_Duplicate409(t *testing.T) {
	creator := &fakeKBCreator{err: store.ErrExists}
	ts := newCreateKBTestServer(t, qdrantCreateFake(http.StatusNotFound), creator)

	code, body := postCreateKB(t, ts.URL, `{"name":"newkb"}`)
	if code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", code, body)
	}
}

// 保留名 global → 400，且在 EnsureCollection 之前短路（不触碰 qdrant/store）。
func TestCreateKB_ReservedNameShortCircuits(t *testing.T) {
	creator := &fakeKBCreator{}
	ts := newCreateKBTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("qdrant must not be touched for reserved name")
		w.WriteHeader(http.StatusInternalServerError)
	}), creator)

	code, body := postCreateKB(t, ts.URL, `{"name":"global"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", code, body)
	}
	if creator.callCount() != 0 {
		t.Fatalf("reserved name must not create kb, got %v", creator.calls)
	}
}
