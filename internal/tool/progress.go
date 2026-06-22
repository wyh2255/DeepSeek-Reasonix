package tool

import "context"

// ProgressFunc 是工具输出的进度回调函数类型。
//
// 长时间运行的工具（如 bash）在执行过程中会产生输出流，
// 通过此回调将每个输出块实时推送到前端，让用户在工具返回之前就能看到进度。
// 典型场景：bash 命令的 stdout/stderr 实时输出到工具卡片。
type ProgressFunc func(chunk string)

// progressKey 是 context 中进度回调的存储键。
type progressKey struct{}

// WithProgress 将进度回调函数附加到 context 上。
//
// Agent 在每次工具调用时设置此回调，确保输出块路由到正确的工具卡片。
// 执行中的工具通过 ProgressFrom 从 context 中取出回调并调用。
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ProgressFrom 从 context 中取出进度回调函数。
//
// 返回 ok=false 表示 context 中没有附加进度回调（如无头测试或运行循环外的调用）。
func ProgressFrom(ctx context.Context) (ProgressFunc, bool) {
	fn, ok := ctx.Value(progressKey{}).(ProgressFunc)
	return fn, ok && fn != nil
}
