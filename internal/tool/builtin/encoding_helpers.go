package builtin

import (
	"os"
	"strings"

	fileenc "reasonix/internal/fileutil/encoding"
)

// readFileEncoded 读取文件并将其编码解码为 UTF-8。
// 返回解码后的内容和检测到的编码类型，以便调用者在写入时重新编码以保留原始字符集。
func readFileEncoded(path string) (content string, enc fileenc.Kind, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	enc, _ = fileenc.Detect(b)
	return string(fileenc.Decode(b, enc)), enc, nil
}

// writeFileEncoded 将内容编码回指定编码格式并写入文件。
func writeFileEncoded(path string, content string, enc fileenc.Kind) error {
	return os.WriteFile(path, fileenc.Encode(content, enc), 0o644)
}

// matchLineEndings 适配编辑的 old/new 文本到 CRLF 文件的行尾风格。
//
// 背景：read_file 使用 bufio.ScanLines 会剥离 '\r'，所以模型生成的多行 old_string
// 只有 LF 行尾，而 Windows/CJK 源文件可能使用 '\r\n'。
// 此函数将搜索和替换文本转换为文件的行尾风格，修复匹配问题而不改写文件的其他行尾。
func matchLineEndings(content, old, new string) (string, string) {
	if strings.Contains(content, old) || !strings.Contains(content, "\r\n") {
		return old, new
	}
	toCRLF := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
	}
	if strings.Contains(content, toCRLF(old)) {
		return toCRLF(old), toCRLF(new)
	}
	return old, new
}
