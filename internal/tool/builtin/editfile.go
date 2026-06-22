package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/tool"
)

func init() { tool.RegisterBuiltin(editFile{}) }

// editFile 实现了 edit_file 工具，在文件中精确替换一个字符串。
//
// 特性：
//   - old_string 必须在文件中恰好出现一次（唯一性保证）
//   - 保留原文件编码
//   - 自动适配行尾风格（CRLF/LF）
//   - 支持工作区边界限制
//
// roots 非空时限制编辑目标在工作区内；workDir 用于解析相对路径。
type editFile struct {
	roots   []string // 可写入的根目录列表
	workDir string   // 相对路径解析基准目录
}

func (editFile) Name() string { return "edit_file" }

func (editFile) Description() string {
	return "Replace an exact string in a file with another. old_string must occur exactly once; add surrounding context to disambiguate. Use for targeted edits instead of rewriting the whole file."
}

func (editFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path"},"old_string":{"type":"string","description":"Exact text to replace (must be unique in the file)"},"new_string":{"type":"string","description":"Replacement text (may be empty to delete)"}},"required":["path","old_string","new_string"]}`)
}

func (editFile) ReadOnly() bool { return false }

func (e editFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if p.OldString == "" {
		return "", fmt.Errorf("old_string is required")
	}
	p.Path = resolveIn(e.workDir, p.Path)
	if err := confine(e.roots, p.Path); err != nil {
		return "", err
	}

	content, enc, err := readFileEncoded(p.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.Path, err)
	}

	old, newStr := matchLineEndings(content, p.OldString, p.NewString)
	switch strings.Count(content, old) {
	case 0:
		return "", fmt.Errorf("old_string not found in %s", p.Path)
	case 1:
		// ok
	default:
		return "", fmt.Errorf("old_string is not unique in %s; add more surrounding context", p.Path)
	}

	updated := strings.Replace(content, old, newStr, 1)
	if err := writeFileEncoded(p.Path, updated, enc); err != nil {
		return "", fmt.Errorf("write %s: %w", p.Path, err)
	}
	return fmt.Sprintf("edited %s", p.Path), nil
}
