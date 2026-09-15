package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 上下文预算防护测试（2026-09-15 线上 11115 故障的回归防线）。

// TestContextBudgetRejectsOversizedRandomCJK 用"最坏情况"内容（随机汉字，实测
// 1.65 bytes/token）构造超过 1,048,576 上限的请求体：必须本地 400
// context_length_exceeded，且不打上游、不轮转、不罚账号。
func TestContextBudgetRejectsOversizedRandomCJK(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 上限放宽到 8MB，确保拦下来的是 token 预算而不是字节上限。
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 64 << 20})

	const prefix = `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	// 需要 est > 1048576 且 est = ceil(bytes/1.6) → bytes 必须 > 1677721；用 1,800,000 字节。
	pad := strings.Repeat("测", (1_800_000-len(prefix)-len(suffix))/3)
	body := prefix + pad + suffix
	if int64(len(body)) <= 1_700_000 { // 低于此值估算就落回上限内，测的就不是拦截路径了
		t.Fatalf("fixture too small: %d bytes", len(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "context_length_exceeded") {
		t.Errorf("error code missing context_length_exceeded: %s", rec.Body.String())
	}
	if calls != 0 {
		t.Errorf("upstream calls=%d want 0 (must not reach upstream)", calls)
	}
	// 不罚账号：无冷却、无禁用、无错误计数。
	for _, uid := range []string{"u1"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
			t.Errorf("account %s penalized: %+v", uid, st)
		}
	}
}

// TestContextBudgetAllowsUnderLimit 正常大小的请求不受预算预检影响（必须放行到上游）。
func TestContextBudgetAllowsUnderLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 8 << 20})
	// 非流式：fake 上游返回的 sseOK 会被 Aggregate 解析后直接 200 返回（单次调用）。
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (small request must pass)", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestContextBudgetUnknownModelNotBlocked 未收录模型不做本地拦截（宁可放行让上游判定，
// 也不要因估算器缺项误杀正常请求）。
func TestContextBudgetUnknownModelNotBlocked(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 64 << 20})
	pad := strings.Repeat("测", 600_000) // ~1.8MB，远超 128k 但模型未收录
	body := `{"model":"brand-new-model-x","messages":[{"role":"user","content":"` + pad + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (unknown model must not be locally blocked)", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatContextTooLongFromUpstreamDoesNotRotate 上游返回 11115 时立即终止：
// 不轮转（calls 必须为 1，即使池里有 3 个账号）、不罚号、回传 400 而非 503。
func TestChatContextTooLongFromUpstreamDoesNotRotate(t *testing.T) {
	const tooLong = `{"code":11115,"msg":"prompt is too long: 1055123 tokens > 1048576 maximum","extError":{"code":"context_length_exceeded"}}`
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, tooLong, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 64 << 20})
	// 未收录模型 → 绕过本地预算预检，直达上游并命中 11115 兜底分支。
	body := `{"model":"probe-model","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (not 503); body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1 (must NOT rotate the same oversized body)", calls)
	}
	if !strings.Contains(rec.Body.String(), "context_length_exceeded") {
		t.Errorf("missing context_length_exceeded: %s", rec.Body.String())
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("account penalized on context-too-long: %+v", st)
	}
	for _, uid := range []string{"u2", "u3"} {
		if s, _ := p.Status(uid); s.Cooling || s.Disabled || s.ErrTotal != 0 {
			t.Errorf("untried account %s penalized: %+v", uid, s)
		}
	}
}

// TestClassifyContextTooLongAsBadParams 分类层：11115 归 ErrBadParams（不罚号语义）。
func TestClassifyContextTooLongAsBadParams(t *testing.T) {
	body := `{"code":11115,"msg":"prompt is too long: 1048926 tokens > 1048576 maximum","extError":{"code":"context_length_exceeded"}}`
	if got := upstream.Classify(400, body); got != upstream.ErrBadParams {
		t.Errorf("Classify=%v want ErrBadParams", got)
	}
	if !upstream.IsContextTooLong(body) {
		t.Error("IsContextTooLong=false want true")
	}
	// 11155（结构问题）不应被误判为 context-too-long。
	other := `{"code":11155,"msg":"the reasoning content from the previous turn must be passed back in thinking mode","extError":{"code":"reasoning_content_missing"}}`
	if upstream.IsContextTooLong(other) {
		t.Error("11155 must not be classified as context-too-long")
	}
}

// TestEstimateTokensIsUpperBound 估算器上界性质：用实测校准数据（随机汉字 1.65
// bytes/token 为最坏）验证估算值恒 >= 真实 token 数。
func TestEstimateTokensIsUpperBound(t *testing.T) {
	cases := []struct {
		name       string
		bytes      int
		realTokens int64 // 来自 2026-09-15 上游 usage 实测
	}{
		{"随机汉字 24000B", 24000, 14549},
		{"代码 10600B", 10600, 3282},
		{"中文散文 14700B", 14700, 3221},
	}
	for _, c := range cases {
		got := upstream.EstimateTokensFromBytes(c.bytes)
		if got < c.realTokens {
			t.Errorf("%s: est=%d < real=%d (估算必须是上界)", c.name, got, c.realTokens)
		}
	}
}
