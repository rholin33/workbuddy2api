package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// 上下文预算防护测试。
//
// 这些用例的存在理由是 2026-09-15/16 的两次真实事故：
//  1. 初版按"最坏情况字节数"线性推 token（1.6 bytes/token），把 11 次真实请求误杀；
//  2. 真实请求体里绝大部分是 base64 图片数据（1.81MB 只计 521 token），
//     按字节线性推会高估上千倍 —— 所以必须按内容类型估算，且宁可漏拦不误杀。

func guardHandler(t *testing.T, maxInputTokens int64) (*Handler, *int, *pool.Pool) {
	t.Helper()
	calls := new(int)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		*calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 64 << 20,
		ContextGuard: true, MaxInputTokens: maxInputTokens})
	return h, calls, p
}

// chatBody 用 json.Marshal 构造**合法** JSON 请求体。
// 注意：绝不能用裸字符串拼接文本内容——内容里的换行/引号会让 JSON 非法，
// json.Unmarshal(peek) 失败 → model 为空 → 预检跳过，测试会"因错误的原因通过"。
func chatBody(t *testing.T, model, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func guardPost(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// TestGuardAllowsLargeBase64ImagePayload 回归：真实大请求体（1.8MB 里绝大部分是
// base64 图片、上游仅计 521 token）**不得**被本地预检拦下。
func TestGuardAllowsLargeBase64ImagePayload(t *testing.T) {
	h, calls, _ := guardHandler(t, 0)
	// 2.4MB 的 data URL 图片负载
	img := "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 110_000)
	body, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": img}}},
		}},
	})
	if len(body) < 2<<20 {
		t.Fatalf("fixture too small: %d", len(body))
	}
	rec := guardPost(t, h, string(body))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 (base64 图片负载不得被误杀); body=%s", rec.Code, rec.Body.String()[:200])
	}
	if *calls != 1 {
		t.Errorf("upstream calls=%d want 1", *calls)
	}
}

// TestGuardAllowsLargeTextPayloadWithinLimit 回归：1.8MB 的**纯文本**请求体
// （用户实测约 163k token，未超 300k 预算）不得被拦。
func TestGuardAllowsLargeTextPayloadWithinLimit(t *testing.T) {
	h, calls, _ := guardHandler(t, 0)
	// 1.8MB 英文/代码类文本 → 按 4.5 bytes/token 估 ≈ 400k；deepseek 上限 1048576 → 放行
	pad := strings.Repeat("function add(a,b){return a+b} // comment line for padding\n", 32_000)
	body := chatBody(t, "deepseek-v4.1-flash", pad)
	if len(body) < 1<<20 {
		t.Fatalf("fixture too small: %d", len(body))
	}
	rec := guardPost(t, h, body)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 (正常大文本不得被误杀); body=%s", rec.Code, rec.Body.String()[:200])
	}
	if *calls != 1 {
		t.Errorf("upstream calls=%d want 1", *calls)
	}
}

// TestGuardRejectsClearlyOversizedText 真正明显超限的纯文本请求应被本地 400 拦下
// （省掉 3 次注定失败的上游往返），且不打上游、不罚号。
func TestGuardRejectsClearlyOversizedText(t *testing.T) {
	h, calls, pa := guardHandler(t, 0) // deepseek 上限 1048576
	// 6MB 文本 → 6/4.5 ≈ 1.4M token > 1048576
	pad := strings.Repeat("the quick brown fox jumps over the lazy dog 0123456789\n", 105_000)
	body := chatBody(t, "deepseek-v4.1-flash", pad)
	if len(body) < 5<<20 {
		t.Fatalf("fixture too small: %d", len(body))
	}
	rec := guardPost(t, h, body)
	if rec.Code != http.StatusBadRequest {
		t.Logf("debug: limit=%d est=%d bytes=%d guard=%v",
			h.inputLimitFor("deepseek-v4.1-flash"), upstream.EstimateTokens([]byte(body)), len(body), h.cfg.ContextGuard)
		t.Fatalf("code=%d want 400 for clearly-oversized body", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "context_length_exceeded") {
		t.Errorf("missing context_length_exceeded: %s", rec.Body.String())
	}
	if *calls != 0 {
		t.Errorf("upstream calls=%d want 0", *calls)
	}
	st, _ := pa.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("account penalized: %+v", st)
	}
}

// TestGuardRespectsMaxInputTokensOverride cfg.MaxInputTokens 覆盖生效。
func TestGuardRespectsMaxInputTokensOverride(t *testing.T) {
	h, calls, _ := guardHandler(t, 100_000) // 统一上限 100k
	pad := strings.Repeat("hello world padding text\n", 30_000) // ~700KB → 估 ≈155k > 100k
	body := chatBody(t, "glm-5.1", pad)
	rec := guardPost(t, h, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (global override 100k)", rec.Code)
	}
	if *calls != 0 {
		t.Errorf("upstream calls=%d want 0", *calls)
	}
}

// TestGuardSkippedForUnknownModel 未收录模型不做本地拦截（宁可放行让上游判定）。
func TestGuardSkippedForUnknownModel(t *testing.T) {
	h, calls, _ := guardHandler(t, 0)
	body := chatBody(t, "brand-new-model-x", strings.Repeat("x", 3_000_000))
	rec := guardPost(t, h, body)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 (未知模型不得本地拦截)", rec.Code)
	}
	if *calls != 1 {
		t.Errorf("upstream calls=%d want 1", *calls)
	}
}

// TestGuardSmallBodySkipsEstimate 512KB 以下的请求体直接跳过估算（热路径零开销）。
func TestGuardSmallBodySkipsEstimate(t *testing.T) {
	h, calls, _ := guardHandler(t, 1000) // 极小上限，但请求体也很小 → 不该被拦
	body := `{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`
	rec := guardPost(t, h, body)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 (小请求体跳过预检)", rec.Code)
	}
	if *calls != 1 {
		t.Errorf("upstream calls=%d want 1", *calls)
	}
}

// TestChatContextTooLongFromUpstreamDoesNotRotate 上游返回 11115 时立即终止：
// 不轮转、不罚号、回传 400 而非 503（本地漏拦时的兜底路径）。
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
	body := `{"model":"probe-model","messages":[{"role":"user","content":"hi"}]}`
	rec := guardPost(t, h, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (not 503); body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1 (must NOT rotate)", calls)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("account penalized on context-too-long: %+v", st)
	}
}

// TestClassifyContextTooLongAsBadParams 分类层：11115 归 ErrBadParams。
func TestClassifyContextTooLongAsBadParams(t *testing.T) {
	body := `{"code":11115,"msg":"prompt is too long: 1048926 tokens > 1048576 maximum","extError":{"code":"context_length_exceeded"}}`
	if got := upstream.Classify(400, body); got != upstream.ErrBadParams {
		t.Errorf("Classify=%v want ErrBadParams", got)
	}
	if !upstream.IsContextTooLong(body) {
		t.Error("IsContextTooLong=false want true")
	}
	other := `{"code":11155,"msg":"the reasoning content from the previous turn must be passed back in thinking mode","extError":{"code":"reasoning_content_missing"}}`
	if upstream.IsContextTooLong(other) {
		t.Error("11155 must not be classified as context-too-long")
	}
}

// TestEstimateTokensByContentType 估算器按内容类型折算（这是防误杀的核心）。
func TestEstimateTokensByContentType(t *testing.T) {
	// base64 图片负载：1.8MB 应远低于按文本折算的结果（实测上游仅计 521 token）
	blob, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 1_800_000)},
			}},
		}},
	})
	estImg := upstream.EstimateTokens(blob)
	if estImg > 200_000 {
		t.Errorf("base64 图片负载估算 %d 过高（按内容类型折算应 <200k）", estImg)
	}
	// 纯文本 1.8MB → 约 400k
	text, _ := json.Marshal(map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("hello world ", 150_000)}},
	})
	estText := upstream.EstimateTokens(text)
	if estText < 200_000 || estText > 600_000 {
		t.Errorf("纯文本估算 %d 超出预期区间 [200k,600k]", estText)
	}
	if estImg >= estText {
		t.Errorf("图片类估算(%d)应显著小于文本类(%d)", estImg, estText)
	}
}
