package builtin

import (
	"path/filepath"
	"time"

	"reasonix/internal/netclient"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// Workspace 构建绑定到工作目录的内置工具集，使多个 Agent 可以并发运行，
// 各自拥有独立的路径根 — 例如桌面前端为每个项目打开一个标签页。
//
// 进程工作目录是全局的，无法按 Agent 独立设置（os.Chdir 是进程级别的），
// 因此每个工具改为基于此目录解析相对路径，bash 也在此目录中运行。
//
// 字段说明：
//   - Dir: 工作目录（空值产生与编译时内置工具完全相同的进程 cwd 工具）
//   - WriteRoots: 文件写入工具的限制目录（见 ConfineWriters）；
//     空且 Dir 非空时，Dir 本身成为唯一的写入根
//   - Bash: bash 工具的 OS 沙箱规范（见 ConfineBash）
//   - BashTimeout: bash 前台命令的超时时间
//   - Search: grep 搜索引擎配置（ripgrep 或原生 Go 扫描器）
//   - ProxySpec: web_fetch 的代理设置
type Workspace struct {
	Dir         string              // 工作目录
	WriteRoots  []string            // 文件写入限制目录
	Bash        sandbox.Spec        // bash OS 沙箱规范
	BashTimeout time.Duration       // bash 前台命令超时
	Search      SearchSpec          // grep 搜索引擎配置
	ProxySpec   netclient.ProxySpec // web_fetch 代理设置
}

// Tools 返回绑定到工作区的内置工具集，可直接添加到每次运行的 tool.Registry。
//
// 参数：
//   - enabled: 要启用的工具名称列表。为空时返回所有内置工具；
//     非空时只返回指定名称的工具（未知名称被忽略）
//
// 这是 CLI 进程 cwd 组装的工作区版本 — 桌面驱动为每个 Agent 调用一次，
// 而非依赖全局工作目录。
func (w Workspace) Tools(enabled ...string) []tool.Tool {
	writeRoots := w.WriteRoots
	if len(writeRoots) == 0 && w.Dir != "" {
		writeRoots = []string{w.Dir}
	}
	roots := realRoots(writeRoots)

	overrides := map[string]tool.Tool{
		"read_file":     readFile{workDir: w.Dir},
		"write_file":    writeFile{workDir: w.Dir, roots: roots},
		"edit_file":     editFile{workDir: w.Dir, roots: roots},
		"multi_edit":    multiEdit{workDir: w.Dir, roots: roots},
		"move_file":     moveFile{workDir: w.Dir, roots: roots},
		"notebook_edit": notebookEdit{workDir: w.Dir, roots: roots},
		"delete_range":  deleteRange{workDir: w.Dir, roots: roots},
		"delete_symbol": deleteSymbol{workDir: w.Dir, roots: roots},
		"code_index":    codeIndex{workDir: w.Dir},
		"bash":          bash{workDir: w.Dir, sb: w.Bash, timeout: w.BashTimeout},
		"ls":            listDir{workDir: w.Dir},
		"glob":          globTool{workDir: w.Dir},
		"grep":          grepTool{workDir: w.Dir, rg: w.Search.RgPath},
		"web_fetch":     webFetch{proxySpec: w.ProxySpec},
	}
	all := tool.Builtins()
	if len(enabled) == 0 {
		for i, t := range all {
			if bound, ok := overrides[t.Name()]; ok {
				all[i] = bound
			}
		}
		return all
	}
	want := make(map[string]bool, len(enabled))
	for _, n := range enabled {
		want[n] = true
	}
	out := make([]tool.Tool, 0, len(enabled))
	for _, t := range all {
		if want[t.Name()] {
			if bound, ok := overrides[t.Name()]; ok {
				t = bound
			}
			out = append(out, t)
		}
	}
	return out
}

// resolveIn 将工具的路径/模式参数映射到工作目录。
//
// 规则：
//   - workDir 为空：返回 p 不变（编译时内置工具的进程 cwd 行为）
//   - p 为空或 "."：返回 workDir 本身（ls/grep 的默认 "." 指向工作区根）
//   - p 是绝对路径：原样返回（显式绝对路径被尊重 — 工作区边界由 write-confiner 强制）
//   - p 是相对路径：与 workDir 拼接
func resolveIn(workDir, p string) string {
	if workDir == "" {
		return p
	}
	if p == "" || p == "." {
		return workDir
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workDir, p)
}

// vendorDirs are directory names grep and glob skip during a recursive walk:
// dependency, VCS, and build-cache trees that almost never hold the searched
// source and would otherwise dominate the walk (node_modules alone can be 100k+
// files) and fill the result cap with noise. Only skipped when nested — a walk
// rooted directly at one (an explicit `grep node_modules`) still searches it.
var vendorDirs = map[string]bool{
	".git": true, ".svn": true, ".hg": true, ".jj": true,
	"node_modules": true, "vendor": true, ".venv": true,
	"__pycache__": true, ".mypy_cache": true, ".pytest_cache": true,
}

// skipWalkDir reports whether a directory should be pruned from a recursive walk
// rooted at root. The root itself is never pruned, so explicitly targeting a
// vendor dir still works.
func skipWalkDir(root, path, name string) bool {
	if path == root {
		return false
	}
	return vendorDirs[name] || isProtectedDir(absClean(path))
}
