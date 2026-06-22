package builtin

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/netclient"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// ConfineBash 返回绑定了 OS 沙箱规范的 bash 内置工具，覆盖 init 注册的无限制实例。
// 当 spec 启用沙箱时，bash 通过沙箱执行每个命令。
// 可选的 timeout 参数设置前台命令的超时时间。
func ConfineBash(spec sandbox.Spec, timeout ...time.Duration) tool.Tool {
	shell := spec.Shell
	if shell.Path == "" {
		shell = sandbox.ResolveShell("", "", nil)
	}
	b := bash{sb: spec, shell: shell}
	if len(timeout) > 0 {
		b.timeout = timeout[0]
	}
	return b
}

// ConfineWebFetch 返回绑定了代理设置的 web_fetch 内置工具，同时保留 SSRF 防护。
func ConfineWebFetch(proxySpec netclient.ProxySpec) tool.Tool {
	return webFetch{proxySpec: proxySpec}
}

// ConfineWriters 返回绑定了工作区边界的文件写入内置工具集合。
// 包括 write_file、edit_file、multi_edit、move_file、notebook_edit、delete_range、delete_symbol。
//
// roots 是唯一允许修改的目录列表。组合根（composition root）将这些工具添加到
// 每次运行的注册表中，覆盖 init 时注册的无限制实例，使写入默认限制在工作区内。
// roots 可以是相对路径，在此处解析为绝对路径。空 roots 表示无限制。
func ConfineWriters(roots []string) []tool.Tool {
	rs := realRoots(roots)
	return []tool.Tool{
		writeFile{roots: rs},
		editFile{roots: rs},
		multiEdit{roots: rs},
		moveFile{roots: rs},
		notebookEdit{roots: rs},
		deleteRange{roots: rs},
		deleteSymbol{roots: rs},
	}
}

// realRoots resolves each root to an absolute, symlink-free path, dropping any
// that cannot be made absolute. Resolving here (once) means the per-call check
// only has to resolve the target.
func realRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if real, err := realPath(r); err == nil {
			out = append(out, real)
		}
	}
	return out
}

// confine 检查目标路径是否在允许的根目录内，不在则返回错误。
// 空 roots 表示无限制（返回 nil）— 这是运行前配置工作区之前的安全默认值。
// 错误信息面向模型编写：指出边界和如何扩大范围。
func confine(roots []string, target string) error {
	if len(roots) == 0 {
		return nil
	}
	abs, err := realPath(target)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", target, err)
	}
	for _, r := range roots {
		if within(r, abs) {
			return nil
		}
	}
	return fmt.Errorf("path %q is outside the writable roots (writes are confined to %s); "+
		"write inside the workspace or a configured allow_write root, or widen [sandbox] workspace_root / allow_write in reasonix.toml",
		target, strings.Join(roots, ", "))
}

// realPath 将路径解析为绝对、无符号链接的形式。
// 因为写入目标可能尚不存在（write_file 会创建它），所以对最深的已存在祖先
// 使用 EvalSymlinks，然后重新附加尚不存在的尾部路径。
// 这防止了通过符号链接目录将写入操作走私到根目录之外。
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	tail := ""
	cur := abs
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil // nothing along the path exists; use the cleaned abs
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// within 报告 path 是否在 root 下或与 root 相同。
// 两者必须是绝对、已清理、无符号链接的路径。
// 使用 filepath.Rel 确保跨卷正确性，不会被仅匹配部分路径组件的前缀欺骗
//（例如 /work-other 不在 /work 内）。
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
