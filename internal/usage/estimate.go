// Package usage W3-② token 计费工具：本地估算 + 计价
package usage

// EstimateTokens 启发式估算文本的 token 数（上游流式响应末尾 usage 缺失时的兜底）
//
// 为什么需要估算（面试讲点）：
//   非流式响应基本都带 usage；但很多上游的**流式响应在末尾不一定返回 usage**。
//   计费不能因为缺 usage 就漏记，所以用本地估算兜底。
//
// 估算规则（启发式，不用真 tokenizer）：
//   - ASCII（英文/数字/符号）：约 4 个字符 = 1 token
//   - 非 ASCII（中文等）：约 1 个字符 = 1 token（中文 1 字 ≈ 0.6~1 token）
// 误差说明：真实精度要用 tiktoken/cl100k_base，这里保证量级不差太远即可
// （计费口径以 usage 为准，估算只在缺失时兜底）
func EstimateTokens(text string) int {
	ascii := 0
	nonASCII := 0
	for _, r := range text {
		if r <= 0x7F {
			ascii++
		} else {
			nonASCII++
		}
	}
	return ascii/4 + nonASCII
}
