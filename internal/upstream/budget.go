// budget.go 上下文预算预检（出站前的本地早退）。
//
// 背景（2026-09-15 线上故障）：客户端一次性塞入超大历史，上游返回 HTTP 400
// code=11115 "prompt is too long"，网关逐号轮转重试同一份超大 body，每个账号各撞
// 一次 400，最终抛 503 —— 看起来像"账号/额度全废"。本层用于省掉那几次注定失败的往返。
//
// ⚠️ 两条设计铁律（都是血泪教训）：
//
//	1) **宁可漏拦，绝不误杀**。误杀 = 正常请求被本地 400 拒掉（用户可见功能故障，
//	   CPA 不会补救）；漏拦 = 多打一次上游，拿到 11115 后由 handler 的短路分支
//	   直接返回 400（不罚号、不轮转），只是慢一点。代价不对称，所以估算必须偏宽松。
//	2) **绝不能按整体字节数线性推 token**。2026-09-16 实测：一个 1.81MB 的真实请求体
//	   里只有 2 个 input item，绝大部分是 base64 图片数据，上游只计 521 token；
//	   另一个 1.68MB 的请求体用户实测仅 163k token。按字节线性推（旧实现用最坏值
//	   1.6 bytes/token）会把这类请求高估 6~1800 倍，直接误杀 —— 线上连续误杀了
//	   11 次真实请求（23:51~00:40）。
//
// 现行口径：解析 JSON，**逐字段按类型**累加——文本按实测密度折算，二进制/图片类
// （data URL、长 base64 块）按极宽松系数折算。
package upstream

import (
	"encoding/json"
	"math"
	"strings"
)

// BytesPerTokenText 文本类估算系数（bytes/token）。
//
// 实测校准（2026-09-16，上游 usage 回读）：1.95MB 代码+CJK 文本 → prompt_tokens=720526，
// 即真实密度 2.72 bytes/token（越密越危险，越小越保守）。这里取 4.5，比实测**低估 1.65 倍**
// —— 故意的：低估 = 少拦 = 不漏杀真实请求；真正的硬约束由上游 11115 + handler 短路兜底。
// 按 4.5 折算，deepseek(1048576) 的本地预检门槛 ≈ 4.7MB 纯文本，glm/gpt(300k) ≈ 1.35MB 纯文本；
// 带图片/二进制负载时门槛再宽 2.7 倍。
const BytesPerTokenText = 4.5

// BytesPerTokenBinary 二进制/base64 类估算系数。
//
// 实测依据（2026-09-16）：上游对图片按图/尺寸计费而非按 base64 字节数——
// 有效图片 + 1.5MB 尾部垃圾（请求体 2MB）prompt_tokens 仍只有 727；
// https URL 与内联 data: URL **完全等价**（都 200，无形式偏见）。故这类字节几乎不进入 token 预算。
// 取 12 = 比文本口径再宽 2.7 倍：既不会让 base64 负载把估算顶穿造成误杀，
// 也仍能对「纯 base64 灌满 8MB」给出拦截信号。
// （注：图片字节无效时上游报 11133 而非超限，已由 IsClientParamError 单独短路。）
const BytesPerTokenBinary = 12.0

// blobMinBytes 判定"二进制/图片类字符串"的最小长度：短串即使像 base64 也按文本算，
// 避免把普通 token 串误归类。1KB 以上的无空白长串才可能是图片/密文。
const blobMinBytes = 1024

// EstimateTokens 解析请求体并**按字段类型**估算输入 token 数（偏低估，宁可漏拦）。
// 解析失败时回落到按整体字节的文本系数估算（仍比旧实现宽松 2.8 倍）。
func EstimateTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	var obj any
	if err := json.Unmarshal(body, &obj); err != nil {
		return EstimateTokensFromBytes(len(body))
	}
	var total float64
	walkStrings(obj, func(s string) {
		total += tokensForString(s)
	})
	return int64(math.Ceil(total))
}

// tokensForString 单个字符串的 token 估算：图片/base64 类走宽松系数，文本走实测系数。
func tokensForString(s string) float64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	if isBinaryLike(s) {
		return float64(n) / BytesPerTokenBinary
	}
	return float64(n) / BytesPerTokenText
}

// isBinaryLike 判断字符串是否为二进制/base64 类负载（data URL、长 base64 块、无空白长串）。
func isBinaryLike(s string) bool {
	if len(s) < blobMinBytes {
		return false
	}
	if strings.HasPrefix(s, "data:") { // data:image/png;base64,....
		return true
	}
	// 无空白的长串且以 base64/hex 字符集为主 → 视为二进制编码负载。
	if strings.ContainsAny(s, " \t\n\r") {
		return false
	}
	alpha := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '+', c == '/', c == '=', c == '-', c == '_':
			alpha++
		}
	}
	return float64(alpha)/float64(len(s)) > 0.98
}

// walkStrings 递归遍历 JSON 结构，对每个字符串值调用 fn。
// 只统计 value（内容），不计 key（字段名）——字段名属结构性开销，占比极小。
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	case map[string]any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	}
}

// EstimateTokensFromBytes 按整体字节的**文本**系数估算（兜底路径，JSON 解析失败时使用）。
// 注意这是偏低估的估算，不是上界。
func EstimateTokensFromBytes(n int) int64 {
	if n <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(n) / BytesPerTokenText))
}
