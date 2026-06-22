package event

import (
	"sync"

	"reasonix/internal/evidence"
	"reasonix/internal/nilutil"
)

// Sync 将一个 Sink 包装为线程安全版本，确保并发的 Emit 调用被串行化。
//
// 背景说明：
// 基础 Sink 契约假设事件是串行发射的——代理的运行循环一次只发射一个事件。
// 但是后台任务（internal/jobs）从它们自己的 goroutine 中发射事件，
// 这可能与正在运行的回合的发射重叠。
// 通过将会话 Sink 用 Sync 包装一次，可以保持每个 Sink 都依赖的串行发射不变量
// （无论是 SSE 写入器、Webview 的 EventsEmit 还是 TUI 的 channel），
// 而无需每个 Sink 自己实现加锁逻辑。
//
// 如果传入的 Sink 为 nil，则返回 Discard（丢弃所有事件）。
func Sync(s Sink) Sink {
	if nilutil.IsNil(s) {
		return Discard
	}
	return &syncSink{inner: s}
}

// syncSink 是 Sink 的线程安全包装器，通过互斥锁确保串行访问。
type syncSink struct {
	mu    sync.Mutex // 保护对内部 Sink 的并发访问
	inner Sink       // 被包装的实际 Sink 实现
}

// Emit 在持有互斥锁的情况下将事件转发给内部 Sink。
func (s *syncSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner.Emit(e)
}

// RecordReadinessAudit 在持有互斥锁的情况下将就绪性审计回执转发给内部 Sink。
// 只有当内部 Sink 实现了 ReadinessAuditSink 接口时才会转发。
func (s *syncSink) RecordReadinessAudit(a evidence.ReadinessAudit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rs, ok := s.inner.(ReadinessAuditSink); ok {
		rs.RecordReadinessAudit(a)
	}
}
