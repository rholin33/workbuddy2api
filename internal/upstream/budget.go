// budget.go 上下文预算防护：
//
// 背景（issue：2026-09-15 线上故障）：客户端一次性塞入超大历史，请求体字节数
// 远未触及 server.max_body_mb（8MB），但实际 token 数已超上游模型上限
// （deepseek-v4.1-flash = 1,048,576）。上游返回 HTTP 400 code=11115
// "prompt is too long"，网关此时已把请求打到上游并轮转重试——每个账号都撞同一个
// 400，最终向上抛 503 no_healthy_account，客户端侧表现为 CPA 502
// "upstream stream closed before [DONE]"，看起来像"账号/额度全废"。
//
// 事实是：这是**客户端侧的请求过大问题**，与账号健康、积分余额无关。
// 本文件在出站前用字节保守估算 token，超限直接本地 400 返回明确的
// context_length_exceeded，不打上游、不罚账号、不轮转。
//
// 估算口径来自 2026-09-15 对上游真实 usage 的实测校准（bytes/token）：
//
//	随机汉字（最坏情况）  1.65
//	代码 / ASCII          3.23
//	中文散文              4.56
//	重复文本              5.2+（会骗过估算，故不作为依据）
//
// 因此取**全局最坏值 1.65 bytes/token** 作为保守系数：随机中文会被精确判定，
// 英文/代码类内容会被高估约 2x（偏向早拦、绝不漏拦上游真实超限）。
// 宁可让客户端收到一条清晰的本地 400，也不要放行后打穿整池账号。
package upstream

import "math"

// TokensPerByteFloor 最坏情况下的 bytes/token（随机汉字实测 1.65，取整到 1.6 更保守）。
// 估算公式：estTokens = len(body) / TokensPerByteFloor，恒为真实 token 数的上界。
const TokensPerByteFloor = 1.6

// EstimateTokensFromBytes 用字节长度保守估算 token 数（上界）。
// 恒有 EstimateTokensFromBytes(len(body)) >= 真实 prompt token 数。
func EstimateTokensFromBytes(n int) int64 {
	if n <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(n) / TokensPerByteFloor))
}
