package server

import (
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 上游「请求参数不合法」（HTTP 400 code=11133 / model_param_invalid）的处置回归。
//
// 事故背景（2026-09-16 实测）：图片字节无效（截断 PNG、随机 base64 垃圾）等客户端
// 内容问题会让上游返回 11133。旧实现把它当通用 4xx → 轮转 3 个账号各浪费一次上游
// 往返 → 最终抛出 503 no_healthy_account，客户端误判成「账号全废 / 额度用尽」。
// 实测数据（deepseek-v4.1-flash / gpt-5.3-codex 一致）：
//   - 有效图片 https URL / 内联 data: URL → 均 200（形式无差别）
//   - 无效图片字节 → 11133；有效图 + 1.5MB 尾部垃圾（2MB 体）→ 200
//
// 结论：这是「内容本身不合法」，与账号无关，换号必同样失败 → 必须短路成 400。
func TestClientParamErrorShortCircuits(t *testing.T) {
	var calls int
	const upstreamErr = `{"code":11133,"msg":"Invalid request parameters","requestId":"r1",` +
		`"extError":{"code":"model_param_invalid"}}`
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 400, upstreamErr, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 64 << 20, ContextGuard: true})

	rec := guardPost(t, h, chatBody(t, "deepseek-v4.1-flash", "这段消息带了一张被截断的图片"))

	if rec.Code != 400 {
		t.Fatalf("code=%d want 400（上游参数错误必须原样回 400，不能升级成 503）body=%s",
			rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1（同一份 body 换号必同样 400，不得轮转白打上游）", calls)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "invalid_request_error") {
		t.Errorf("响应缺少明确错误语义 invalid_request_error: %s", got)
	}
	if strings.Contains(got, "no_healthy_account") {
		t.Errorf("不得伪装成 no_healthy_account（会让客户端误判账号全废）: %s", got)
	}
	if !strings.Contains(got, "11133") {
		t.Errorf("响应应带上游原文便于定位: %s", got)
	}
}

// 分类层：11133 必须归 ErrBadParams（不罚账号），但 IsClientParamError 为真（短路用）；
// 11115（超上下文）不得被误判成参数错误，两者走不同分支。
func TestIsClientParamErrorAndClassify11133(t *testing.T) {
	param := `{"code":11133,"msg":"Invalid request parameters","extError":{"code":"model_param_invalid"}}`
	if !upstream.IsClientParamError(param) {
		t.Fatal("11133 应被判定为与账号无关的请求参数错误")
	}
	if k := upstream.Classify(400, param); k != upstream.ErrBadParams {
		t.Errorf("11133 应归 ErrBadParams（不罚号），got %v", k)
	}
	if upstream.IsClientParamError(`{"code":11115,"msg":"prompt is too long"}`) {
		t.Error("11115 是上下文超限，不应被判成参数错误（走 IsContextTooLong 分支）")
	}
	if upstream.IsClientParamError(`{"code":11102,"msg":"service info not found"}`) {
		t.Error("11102 是模型不存在，不应被判成参数错误")
	}
}
