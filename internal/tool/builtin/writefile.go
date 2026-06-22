package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"reasonix/internal/tool"
)

func init() { tool.RegisterBuiltin(writeFile{}) }

// writeFile 实现了 write_file 工具，将内容写入文件（覆盖现有内容）。
//
// 特性：
//   - 自动创建父目录
//   - 保留原文件编码（GBK/UTF-16/BOM）而非强制写入 UTF-8
//   - 内容未变化时跳过写入
//   - 支持工作区边界限制（roots）
//
// roots 非空时限制写入目标在工作区内（见 confine）；init 注册的零值无限制，
// 运行时通过 ConfineWriters 覆盖。
// workDir 非空时用于解析相对路径。
type writeFile struct {
	roots   []string // 可写入的根目录列表（工作区边界）
	workDir string   // 相对路径解析基准目录
}

func (writeFile) Name() string { return "write_file" }

func (writeFile) Description() string {
	return "Write content to a file at the given path (overwriting existing content). Creates parent directories as needed."
}

func (writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path"},"content":{"type":"string","description":"Full content to write"}},"required":["path","content"]}`)
}

func (writeFile) ReadOnly() bool { return false }

func (w writeFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	p.Path = resolveIn(w.workDir, p.Path)
	if err := confine(w.roots, p.Path); err != nil {
		return "", err
	}
	// Preserve the existing file's encoding (GBK/UTF-16/BOM) on overwrite instead
	// of always writing UTF-8, which would silently corrupt a non-UTF-8 file.
	// readFileEncoded returns enc=UTF8 for a missing file — the right default for
	// a newly created one.
	existing, enc, rerr := readFileEncoded(p.Path)
	if rerr == nil && existing == p.Content {
		return fmt.Sprintf("%s already contains the exact content; no changes made", p.Path), nil
	}
	if dir := filepath.Dir(p.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if err := writeFileEncoded(p.Path, p.Content, enc); err != nil {
		return "", fmt.Errorf("write %s: %w", p.Path, err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(p.Content), p.Path), nil
}
