// Package tool 定义了工具（Tool）抽象接口和注册表（Registry）。
//
// 工具系统是 Reasonix 的核心子系统之一，负责管理 AI 模型可调用的所有能力。
// 内置工具（builtin）通过 init() 函数自注册到全局 builtins 映射中；
// 插件工具（如 MCP 工具）在运行时添加到每个会话的 Registry 中。
// Agent 只与 *Registry 交互，不直接接触全局内置工具集合。
//
// 架构概览：
//   - Tool 接口：定义工具的名称、描述、参数 Schema 和执行逻辑
//   - Previewer 接口：可选能力，让写入工具在不实际修改磁盘的情况下预览变更
//   - 全局 builtins：编译时注册的内置工具集合
//   - Registry：每个运行实例的工具注册表，支持添加、移除、挂起/恢复操作
package tool

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"reasonix/internal/diff"
	"reasonix/internal/provider"
)

// Tool 是模型可以调用的一项能力（工具）的核心接口。
//
// 所有工具（内置工具、MCP 插件工具）都必须实现此接口。
// 模型通过工具名称和 JSON 参数来调用工具，工具执行后返回文本结果供模型继续推理。
type Tool interface {
	// Name 返回工具的唯一名称，用于注册和调用路由。
	// 内置工具使用下划线命名（如 "bash", "read_file", "edit_file"），
	// MCP 工具使用 "mcp__<server>__<tool>" 格式。
	Name() string

	// Description 返回工具的自然语言描述，会传递给模型以便其理解何时使用该工具。
	Description() string

	// Schema 返回工具参数的 JSON Schema，用于模型生成合法的调用参数。
	Schema() json.RawMessage

	// Execute 解析模型生成的原始 JSON 参数，执行工具逻辑，并返回结果文本。
	// 返回的文本会作为工具调用结果反馈给模型，供其继续推理。
	// ctx 携带了进度回调、作业管理器等运行时上下文。
	Execute(ctx context.Context, args json.RawMessage) (string, error)

	// ReadOnly 报告该工具是否对宿主没有可观察的副作用（即只读操作）。
	//
	// Agent 的批量调度逻辑依赖此方法：
	//   - 批次中所有工具都是 ReadOnly 时，并行执行
	//   - 混合批次（包含写入工具）保持顺序执行，确保读写顺序正确
	//
	// bash 和插件工具必须返回 false，因为它们的副作用无法从参数静态推断。
	ReadOnly() bool
}

// Previewer 是写入类工具可选实现的接口，用于在不实际修改磁盘的情况下预览变更。
//
// 前端（如桌面端审批卡片）可以在权限门控决定是否放行之前调用 Preview，
// 向用户展示即将发生的文件变更。通过类型断言 Tool -> Previewer 来发现支持。
// 文件写入类内置工具（write_file, edit_file 等）实现了此接口，大多数工具不会实现。
type Previewer interface {
	// Preview 接收与 Execute 相同的原始 JSON 参数，计算工具"将会做出"的文件变更，
	// 但不实际写入磁盘。返回的 diff.Change 包含变更前后的文本和统一 diff。
	Preview(args json.RawMessage) (diff.Change, error)
}

// PreviewChange 返回写入工具对给定参数将会做出的变更。
//
// 返回 ok=false 的情况：
//   - t 为 nil 或是只读工具
//   - t 未实现 Previewer 接口
//   - 预览出错（通常意味着实际执行也会失败）
//   - 文件是二进制格式（无法渲染 diff）
//
// 参数：
//   - t: 要预览的工具实例
//   - args: 模型生成的原始 JSON 参数
//
// 返回：
//   - diff.Change: 预览的变更信息（包含前后文本和统一 diff）
//   - ok: 是否成功获取到可渲染的预览
func PreviewChange(t Tool, args json.RawMessage) (diff.Change, bool) {
	if t == nil || t.ReadOnly() {
		return diff.Change{}, false
	}
	pv, ok := t.(Previewer)
	if !ok {
		return diff.Change{}, false
	}
	ch, err := pv.Preview(args)
	if err != nil || ch.Binary {
		return diff.Change{}, false
	}
	return ch, true
}

// --- 进程全局的内置工具集合（由 builtin 子包的 init() 填充） ---

// builtins 是编译时注册的所有内置工具的全局映射。
// 每个内置工具在 init() 中通过 RegisterBuiltin 自注册到此映射。
// 启动时 main 包通过空白导入 _ "reasonix/internal/tool/builtin" 触发注册。
var builtins = map[string]Tool{}

// RegisterBuiltin 注册一个编译时内置工具。仅供 init() 使用。
// 如果名称重复会 panic，这是一个编译时的装配错误。
func RegisterBuiltin(t Tool) {
	name := t.Name()
	if _, dup := builtins[name]; dup {
		panic("tool: duplicate built-in " + name)
	}
	builtins[name] = t
}

// Builtins 返回所有已注册的内置工具，按名称排序。
// 用于构建每个会话的初始工具集。
func Builtins() []Tool {
	names := make([]string, 0, len(builtins))
	for n := range builtins {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		out = append(out, builtins[n])
	}
	return out
}

// LookupBuiltin 按名称查找已注册的内置工具。
func LookupBuiltin(name string) (Tool, bool) {
	t, ok := builtins[name]
	return t, ok
}

// --- 每次运行的工具注册表实例 ---

// Registry 是每次运行（会话）的工具集合：包含启用的内置工具和插件工具。
//
// 它是线程安全的（通过 sync.RWMutex 保护），支持：
//   - 添加/替换工具（Add）
//   - 按名称查找（Get）
//   - 按前缀批量移除（RemovePrefix，用于 MCP 服务器断开时清理）
//   - 暂停/恢复前缀（SuspendPrefix/ResumePrefix，防止后台握手覆盖用户禁用）
//   - 导出 Schema 供 provider 使用（Schemas）
type Registry struct {
	mu        sync.RWMutex              // 保护并发访问
	tools     map[string]Tool           // 工具名称 -> 工具实例
	order     []string                  // 插入顺序，保持稳定的遍历顺序
	canon     map[string]json.RawMessage // 规范化后的 Schema 缓存，避免重复序列化
	suspended map[string]bool            // 被暂停的前缀集合，阻止后续 Add
}

// NewRegistry 创建并返回一个空的工具注册表。
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, canon: map[string]json.RawMessage{}, suspended: map[string]bool{}}
}

// Add 向注册表插入（或替换）一个工具，保持首次插入的顺序。
//
// Schema 在此处一次性规范化，注册后不再变化，因此 Schemas()（每轮调用）
// 可以直接复用缓存结果，避免重复序列化。
//
// 如果工具名称匹配已暂停的前缀，则静默忽略（不注册）。
func (r *Registry) Add(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := t.Name()
	for prefix := range r.suspended {
		if strings.HasPrefix(name, prefix) {
			return
		}
	}
	if _, ok := r.tools[name]; !ok {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
	r.canon[name] = provider.CanonicalizeSchema(t.Schema())
}

// MCPNamePrefix 是所有 MCP 工具名称共享的命名空间前缀。
// 模型可见的 MCP 工具名称格式为 "mcp__<server>__<tool>"。
const MCPNamePrefix = "mcp__"

// SplitMCPName 将模型可见的 MCP 工具名称 "mcp__<server>__<tool>" 拆分为
// 服务器名称和工具名称两部分。
//
// 返回 ok=false 的情况：
//   - 非 MCP 工具（内置工具名称不带前缀）
//   - 格式错误的名称（缺少服务器或工具部分）
func SplitMCPName(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, MCPNamePrefix) {
		return "", "", false
	}
	rest := name[len(MCPNamePrefix):]
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// RemovePrefix 注销所有名称以 prefix 开头的工具，返回移除的数量。
// 用于 MCP 服务器断开连接时清理其 "mcp__<server>__" 命名空间下的所有工具。
func (r *Registry) RemovePrefix(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	kept := r.order[:0]
	removed := 0
	for _, name := range r.order {
		if strings.HasPrefix(name, prefix) {
			delete(r.tools, name)
			delete(r.canon, name)
			removed++
			continue
		}
		kept = append(kept, name)
	}
	r.order = kept
	return removed
}

// SuspendPrefix 注销匹配的工具并阻止该前缀的后续 Add 调用，直到 ResumePrefix 被调用。
//
// 用于会话级别的 MCP 禁用：当用户关闭某个 MCP 服务器时，后台可能仍在进行握手，
// 握手完成后会尝试重新注册工具。SuspendPrefix 确保这些工具不会被重新添加。
func (r *Registry) SuspendPrefix(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.suspended[prefix] = true
	kept := r.order[:0]
	removed := 0
	for _, name := range r.order {
		if strings.HasPrefix(name, prefix) {
			delete(r.tools, name)
			delete(r.canon, name)
			removed++
			continue
		}
		kept = append(kept, name)
	}
	r.order = kept
	return removed
}

// ResumePrefix 恢复之前被 SuspendPrefix 暂停的前缀，允许后续的 Add 调用。
func (r *Registry) ResumePrefix(prefix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.suspended, prefix)
}

// Get 按名称查找工具，返回工具实例和是否找到。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	t, ok := r.tools[name]
	return t, ok
}

// Len 返回注册表中的工具数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.order)
}

// Names 按插入顺序返回所有已注册工具的名称。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Schemas 按稳定的名称顺序导出工具定义，供 provider 构建请求时使用。
// 每轮对话都会调用此方法，因此使用缓存的规范化 Schema 避免重复序列化。
func (r *Registry) Schemas() []provider.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, len(r.order))
	copy(names, r.order)
	sort.Strings(names)

	out := make([]provider.ToolSchema, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		if t == nil {
			continue
		}
		out = append(out, provider.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  r.canon[name],
		})
	}
	return out
}
