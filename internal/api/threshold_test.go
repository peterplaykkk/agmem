package api

// 分数阈值（UC-1）与版本端点行为测试。
// 阈值：score < AGMEM_SCORE_THRESHOLD（默认 0.5）的命中不返回，全被过滤时 hits=[]。
// 版本：单库化（2026-09-08）后 /api/version/global 与 /api/version/{kb} 同形状，
// 均返回 cfg.KB 的数值版本（旧「跨 kb 指纹」语义已退役）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sre/agmem/internal/config"
	"sre/agmem/internal/qdrant"
)

// filterHitsByScore 纯函数：阈值边界（=阈值保留，<阈值丢弃）与空入参。
func TestFilterHitsByScore(t *testing.T) {
	hits := []qdrant.SearchHit{
		{ID: "a", Score: 0.9},
		{ID: "b", Score: 0.5},
		{ID: "c", Score: 0.49999},
		{ID: "d", Score: 0.1},
	}
	cases := []struct {
		name      string
		threshold float64
		wantIDs   []string
	}{
		{name: "阈值0.5含等于", threshold: 0.5, wantIDs: []string{"a", "b"}},
		{name: "阈值略高于0.5排除等于", threshold: 0.50001, wantIDs: []string{"a"}},
		{name: "阈值0全保留", threshold: 0, wantIDs: []string{"a", "b", "c", "d"}},
		{name: "阈值1全丢弃但非nil", threshold: 1, wantIDs: []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterHitsByScore(hits, c.threshold)
			if got == nil {
				t.Fatal("filter must return non-nil slice (JSON [] not null)")
			}
			var ids []string
			for _, h := range got {
				ids = append(ids, h.ID)
			}
			if strings.Join(ids, ",") != strings.Join(c.wantIDs, ",") {
				t.Fatalf("want %v, got %v", c.wantIDs, ids)
			}
		})
	}
	t.Run("空入参返回空切片", func(t *testing.T) {
		got := filterHitsByScore(nil, 0.5)
		if got == nil || len(got) != 0 {
			t.Fatalf("want non-nil empty, got %#v", got)
		}
	})
}

// 全被阈值过滤 → 200 且 hits 序列化为 []（非 null）。
func TestAsk_ThresholdAllFilteredHitsEmptyArray(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{scores: map[string]float64{"sre": 0.3}}
	_, ts := newSingleKBTestServerCfg(t, st, qh, config.Config{KB: config.DefaultKB, ScoreThreshold: 0.5})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/ask", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	raw := string(body[:n])
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, `"hits":[]`) {
		t.Fatalf("all-filtered hits must serialize as [] (not null/omitted), got: %s", raw)
	}
	var out struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out.Hits) != 0 {
		t.Fatalf("want empty hits, err=%v out=%s", err, raw)
	}
}

// 阈值过滤经 HTTP 语义复核：score=0.5 与阈值相等 → 保留；0.3 → 丢弃。
func TestAsk_ThresholdBoundary(t *testing.T) {
	cases := []struct {
		name  string
		score float64
		want  int
	}{
		{name: "恰在阈值保留", score: 0.5, want: 1},
		{name: "低于阈值丢弃", score: 0.3, want: 0},
		{name: "高于阈值保留", score: 0.9, want: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newFakeSingleKB()
			qh := &fakeQdrantFanout{scores: map[string]float64{"sre": c.score}}
			_, ts := newSingleKBTestServerCfg(t, st, qh, config.Config{KB: config.DefaultKB, ScoreThreshold: 0.5})
			_, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
			hits, _ := out["hits"].([]any)
			if len(hits) != c.want {
				t.Fatalf("score=%v: want %d hits, got %v", c.score, c.want, out["hits"])
			}
		})
	}
}

// filterHitsByScoreDiag legacy 诊断（G06）。
func TestFilterHitsByScoreDiag(t *testing.T) {
	kept, d := filterHitsByScoreDiag([]qdrant.SearchHit{
		{ID: "a", Score: 0.9}, {ID: "b", Score: 0.2},
	}, 0.5)
	if len(kept) != 1 || kept[0].ID != "a" {
		t.Fatalf("kept = %v", kept)
	}
	if d.Mode != "score" || d.Threshold != 0.5 || d.Kept != 1 || d.Dropped != 1 || d.DroppedReason != "score<threshold" {
		t.Fatalf("diag = %+v", d)
	}
}

// legacy（AGMEM_HYBRID=off）HTTP 路径的过滤诊断：filter.mode=score，
// 与 hybrid 的 dense 口径区分开（G06 口径钉死）。
func TestAsk_LegacyFilterDiag(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{scores: map[string]float64{"sre": 0.3}}
	_, ts := newSingleKBTestServerCfg(t, st, qh,
		config.Config{KB: config.DefaultKB, ScoreThreshold: 0.5, HybridEnabled: false})

	_, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
	f, ok := out["filter"].(map[string]any)
	if !ok {
		t.Fatalf("filter diag missing: %v", out)
	}
	if f["mode"] != "score" || f["rule"] != "score>=threshold" || f["dropped"] != float64(1) {
		t.Fatalf("legacy filter diag wrong: %v", f)
	}
	if hits, _ := out["hits"].([]any); len(hits) != 0 {
		t.Fatalf("want filtered hits, got %v", out["hits"])
	}
}

// GET /api/version/global（单库化兼容端点）：返回 cfg.KB 的数值版本，
// 形状与 /api/version/{kb} 一致；权限走 HasReadAccess（404 掩蔽）。
func TestVersionGlobal_SingleKBVersion(t *testing.T) {
	st := newFakeSingleKB()
	st.version = 7
	_, ts := newSingleKBTestServer(t, st, qhNoop())

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/version/global", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["kb"] != "sre" || out["version"] != float64(7) {
		t.Fatalf("want {kb:sre, version:7}, got %v", out)
	}
	// store 只解析 cfg.KB（旧客户端可传任意 {kb} 路径段，不影响本端点）
	if calls := st.kbCalls(); len(calls) != 1 || calls[0] != "sre" {
		t.Fatalf("store must resolve cfg.KB only, got %v", calls)
	}
}

// GET /api/version/global：OIDC sub 透传权限检查；无权限 404 掩蔽。
func TestVersionGlobal_OIDCSubPassedAndMasked(t *testing.T) {
	st := newFakeSingleKB()
	s, ts := newSingleKBTestServer(t, st, qhNoop())
	sid, err := s.Session.Put("user-9")
	if err != nil {
		t.Fatal(err)
	}
	get := func(url string, cookie *http.Cookie) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, url+"/api/version/global", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, _ := get(ts.URL, &http.Cookie{Name: "agmem_session", Value: sid})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if st.subs[0] != "user-9" {
		t.Fatalf("OIDC sub must be passed to access check, got %q", st.subs[0])
	}
	// 无权限 → 404 掩蔽
	st.accessOK = false
	code, _ = get(ts.URL, &http.Cookie{Name: "agmem_session", Value: sid})
	if code != http.StatusNotFound {
		t.Fatalf("no access: want 404 masked, got %d", code)
	}
}

// kb 保留名：global 与 /api/version/global 字面路由冲突，创建必须 400。
func TestCreateKB_GlobalNameReserved(t *testing.T) {
	s := testServer()
	h := s.Routes()
	req := httptest.NewRequest(http.MethodPost, "/api/kb", strings.NewReader(`{"name":"global"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reserved name must be 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func qhNoop() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
}
