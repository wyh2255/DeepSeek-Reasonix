// Package builtin 提供 Reasonix 的编译时内置工具。
//
// 每个工具通过 init() 函数自注册到 tool 包的全局注册表中。
// main 包通过空白导入 _ "reasonix/internal/tool/builtin" 来触发这些注册。
//
// 内置工具列表：
//   - bash: 执行 shell 命令
//   - read_file: 读取文本文件（带行号）
//   - write_file: 写入文件（覆盖）
//   - edit_file: 精确字符串替换编辑
//   - multi_edit: 批量原子编辑
//   - move_file: 移动/重命名文件
//   - notebook_edit: 编辑 Jupyter 笔记本单元格
//   - delete_range: 删除文本范围
//   - delete_symbol: 删除 Go 源码符号
//   - grep: 正则搜索文件内容
//   - glob: 按模式匹配文件名
//   - ls: 列出目录内容
//   - web_fetch: 获取网页内容
//   - bash_output / kill_shell / wait: 后台作业管理
//   - todo_write: 任务列表管理
//   - complete_step: 完成步骤记录
//   - code_index: 代码符号索引
package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/text/transform"

	fileenc "reasonix/internal/fileutil/encoding"
	"reasonix/internal/tool"
)

const (
	readFileBinaryPeek   = 8 * 1024   // bytes scanned for NUL before reading further
	readFileDetectSample = 256 * 1024 // bytes sampled for encoding detection before streaming
)

func init() { tool.RegisterBuiltin(readFile{}) }

// readFile 实现了 read_file 工具，读取文本文件并返回带行号的内容。
//
// 特性：
//   - 支持 UTF-8、UTF-16（LE/BE）、GBK 等编码自动检测和转换
//   - 输出每行带 1-based 行号前缀（如 "   42→..."），便于后续 edit_file 定位
//   - 支持 offset/limit 分页浏览大文件
//   - 自动拒绝二进制文件（检测 NUL 字节）
//
// workDir 非空时，相对路径基于该目录解析；零值（init 注册时）基于进程工作目录。
type readFile struct{ workDir string }

const (
	readFileDefaultLimit = 2000 // lines returned when limit is unset
)

func (readFile) Name() string { return "read_file" }

func (readFile) Description() string {
	return "Read a text file with optional line offset/limit. Output prefixes each line with its 1-based number (e.g. `   42→...`) so subsequent edit_file calls can target exact lines. Use `offset` and `limit` to page through large files; the tool reports total length and pagination hints in a trailer."
}

func (readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "path":{"type":"string","description":"File path"},
  "offset":{"type":"integer","description":"0-based line offset to start reading from (default 0)","minimum":0},
  "limit":{"type":"integer","description":"Maximum lines to return (default 2000)","minimum":1}
},
"required":["path"]
}`)
}

func (readFile) ReadOnly() bool { return true }

func (r readFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	p.Path = resolveIn(r.workDir, p.Path)
	if p.Offset < 0 {
		p.Offset = 0
	}
	if p.Limit <= 0 {
		p.Limit = readFileDefaultLimit
	}

	// A directory can be os.Open'd but not read as text — catch it up front with
	// an actionable message (and avoid the doubled "read X: read X:" the scanner's
	// error would otherwise produce) so the model switches to the ls tool.
	if info, err := os.Stat(p.Path); err == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a file — use the ls tool to list it, or read a specific file inside it", p.Path)
	}

	f, err := os.Open(p.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.Path, err)
	}
	defer f.Close()

	// Peek the first 8 KiB to reject binary files cheaply (a NUL byte) before
	// reading further — keeps a multi-GB archive from being slurped just to be
	// discarded.
	peek := make([]byte, readFileBinaryPeek)
	pn, perr := io.ReadFull(f, peek)
	peek = peek[:pn]
	peekEOF := perr != nil // whole file fit in the peek (EOF / ErrUnexpectedEOF)

	// BOM check first: UTF-16 files contain 0x00 for every ASCII character, so a
	// naive NUL check would misidentify them as binary.
	switch fileenc.DetectQuick(peek) {
	case fileenc.UTF16LE, fileenc.UTF16BE:
		// UTF-16 is not self-synchronising and can't be streamed line-by-line, so
		// buffer it fully (these files are rare and usually small).
		rest, rerr := io.ReadAll(f)
		if rerr != nil {
			return "", fmt.Errorf("read %s: %w", p.Path, rerr)
		}
		all := append(peek, rest...)
		bom := fileenc.DetectQuick(all)
		return r.scan(bytes.NewReader(fileenc.Decode(all, bom)), p.Offset, p.Limit)
	case fileenc.UTF8BOM:
		// Strip the 3-byte BOM; the content is valid UTF-8 and streams directly.
		body := peek
		if len(body) >= 3 {
			body = body[3:]
		}
		return r.scan(io.MultiReader(bytes.NewReader(body), f), p.Offset, p.Limit)
	}

	// BOM-less UTF-16 (Windows source files) has a NUL for every ASCII char but
	// no BOM, so it reaches here; recognise it by its NUL pattern and decode it
	// rather than rejecting it as binary.
	if k, ok := fileenc.DetectUTF16NoBOM(peek); ok {
		rest, rerr := io.ReadAll(f)
		if rerr != nil {
			return "", fmt.Errorf("read %s: %w", p.Path, rerr)
		}
		all := append(peek, rest...)
		return r.scan(bytes.NewReader(fileenc.Decode(all, k)), p.Offset, p.Limit)
	}

	if bytes.IndexByte(peek, 0) >= 0 {
		return "", fmt.Errorf("binary file %s (NUL byte detected); use `bash hexdump` or another tool", p.Path)
	}

	// Read up to a bounded sample for encoding detection, then stream the rest —
	// so a large text file isn't slurped whole just to return a few lines.
	head := peek
	if !peekEOF {
		more := make([]byte, readFileDetectSample-len(peek))
		mn, merr := io.ReadFull(f, more)
		head = append(peek, more[:mn]...)
		peekEOF = merr != nil
	}

	// Detect from a char-safe slice: when more file follows, trim to the last
	// newline so the sample never ends mid multi-byte sequence (UTF-8 and GB18030
	// are ASCII-transparent, so '\n' is always a clean boundary).
	sample := head
	if !peekEOF {
		if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
			sample = head[:i+1]
		}
	}
	enc, _ := fileenc.Detect(sample)

	src := io.MultiReader(bytes.NewReader(head), f)
	if dec := fileenc.Decoder(enc); dec != nil {
		return r.scan(transform.NewReader(src, dec), p.Offset, p.Limit)
	}
	return r.scan(src, p.Offset, p.Limit)
}

// scan 从 src 读取行并返回带行号的格式化输出。
// offset 是 0-based 起始行偏移，limit 是最大返回行数。
// 超出 limit 的行不会继续读取（避免为计数而读完整个文件）。
func (r readFile) scan(src io.Reader, offset, limit int) (string, error) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var collected []string
	lineNo := 0
	hasMore := false
	for scanner.Scan() {
		lineNo++
		if lineNo <= offset {
			continue
		}
		if len(collected) < limit {
			collected = append(collected, scanner.Text())
			continue
		}
		// A line past the requested window exists — stop here rather than reading
		// the rest of the file just to count the remainder.
		hasMore = true
		break
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan: %w", err)
	}

	if lineNo == 0 {
		return "(empty file)", nil
	}
	if len(collected) == 0 {
		return fmt.Sprintf("(offset %d is past EOF — file has %d lines)", offset, lineNo), nil
	}

	maxShown := offset + len(collected)
	w := len(fmt.Sprint(maxShown))

	var b strings.Builder
	for i, line := range collected {
		fmt.Fprintf(&b, "%*d→%s\n", w, offset+i+1, line)
	}
	if hasMore {
		fmt.Fprintf(&b, "\n[more lines below; pass offset=%d to continue]\n", offset+len(collected))
	}
	return b.String(), nil
}
