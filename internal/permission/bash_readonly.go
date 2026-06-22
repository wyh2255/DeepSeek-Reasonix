package permission

import "strings"

// readOnlyBashCommands 是被认为只读的命令集合。
// 这些命令不会修改文件系统状态、网络状态或进程状态。
// 每个条目是 bash 命令的第一个单词（小写形式）。
// 不在此集合中但也可能是只读的命令（如 "git log"）由 isReadOnlyBashSubject 单独处理。
var readOnlyBashCommands = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
	"ls": true, "find": true, "locate": true, "which": true, "whereis": true, "type": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	"echo": true, "printf": true,
	"pwd": true, "cd": true, "whoami": true, "id": true, "uname": true, "hostname": true,
	"date": true, "env": true, "printenv": true,
	"wc": true, "sort": true, "uniq": true, "cut": true, "tr": true,
	"stat": true, "file": true, "du": true, "df": true,
	"ps": true, "top": true, "htop": true,
	"diff": true, "cmp": true, "comm": true,
	"man": true, "info": true, "help": true,
	"true": true, "false": true, "test": true, "[": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
}

// readOnlyBashPrefixes 是需要检查第二个单词来判断只读状态的命令前缀集合。
// 外层键是主命令（如 "git"），内层键是只读子命令（如 "log", "status"）。
var readOnlyBashPrefixes = map[string]map[string]bool{
	"git": {
		"log": true, "status": true, "diff": true, "show": true,
		"tag":   true,
		"blame": true, "grep": true, "ls-files": true, "ls-tree": true,
		"rev-parse": true, "rev-list": true, "describe": true, "reflog": true,
		"shortlog": true, "whatchanged": true, "cherry": true,
		"cat-file": true, "for-each-ref": true, "name-rev": true,
	},
	"go": {
		"vet": true, "doc": true, "list": true,
		"version": true, "env": true,
	},
	"npm": {
		"ls": true, "list": true, "view": true, "info": true,
		"outdated": true, "audit": true,
	},
	"cargo": {
		"check": true, "doc": true, "search": true,
	},
	"docker": {
		"ps": true, "images": true, "inspect": true, "logs": true,
		"stats": true, "info": true, "version": true,
	},
	"kubectl": {
		"get": true, "describe": true, "logs": true, "explain": true,
		"api-resources": true, "api-versions": true,
	},
}

// isReadOnlyBashSubject 判断一个 bash 命令是否是已知的只读操作。
// subject 是通过 Subject() 从 JSON 参数中提取的值 — 对于 bash 就是原始命令字符串。
//
// 判断逻辑：
//  1. 检查是否包含 shell 语法（管道、重定向等），包含则非只读
//  2. 检查第一个单词是否在 readOnlyBashCommands 集合中
//  3. 检查是否匹配 readOnlyBashPrefixes 中的前缀+子命令组合
//  4. 额外检查危险参数（如 find -exec, sort -o 等会破坏只读性）
func isReadOnlyBashSubject(subject string) bool {
	cmd := strings.TrimSpace(subject)
	if cmd == "" {
		return false
	}
	if containsShellSyntax(cmd) {
		return false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	base := strings.ToLower(fields[0])

	// Check single-word read-only commands.
	if readOnlyBashCommands[base] {
		return !hasUnsafeReadOnlyArgs(base, fields[1:])
	}

	// Check prefix commands (git log, go vet, etc.).
	if len(fields) > 1 {
		if sub, ok := readOnlyBashPrefixes[base]; ok {
			subcmd := strings.ToLower(fields[1])
			return sub[subcmd] && !hasUnsafePrefixArgs(base, subcmd, fields[2:])
		}
	}
	return false
}

// containsShellSyntax 检查命令是否包含 shell 特殊语法（管道、重定向、命令替换等）。
// 包含这些语法的命令不能被视为只读，因为它们可能产生不可预测的副作用。
func containsShellSyntax(cmd string) bool {
	return strings.ContainsAny(cmd, ";|&<>\n`") || strings.Contains(cmd, "$(")
}

// hasUnsafeReadOnlyArgs 检查看似只读的命令是否携带了危险参数。
// 例如 "find -exec" 虽然 find 本身是只读命令，但 -exec 参数会执行外部命令。
func hasUnsafeReadOnlyArgs(base string, args []string) bool {
	switch base {
	case "find":
		return hasAnyArg(args, "-exec", "-execdir", "-delete")
	case "sed":
		for _, arg := range args {
			if strings.HasPrefix(arg, "-i") || strings.HasPrefix(arg, "--in-place") {
				return true
			}
		}
	case "sort":
		return hasArgWithPrefix(args, "-o") || hasAnyArg(args, "--output") || hasArgWithPrefix(args, "--output=")
	}
	return false
}

// hasUnsafePrefixArgs 检查前缀命令的子命令是否携带了危险参数。
// 例如 "git diff --output" 会写入文件，破坏只读性。
func hasUnsafePrefixArgs(base, subcmd string, args []string) bool {
	switch base {
	case "git":
		switch subcmd {
		case "diff", "show", "log":
			return hasAnyArg(args, "--output") || hasArgWithPrefix(args, "--output=")
		}
	case "go":
		if subcmd == "env" {
			return hasAnyArg(args, "-w", "-u")
		}
	}
	return false
}

func hasArgWithPrefix(args []string, prefix string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

func hasAnyArg(args []string, unsafe ...string) bool {
	for _, arg := range args {
		for _, candidate := range unsafe {
			if arg == candidate {
				return true
			}
		}
	}
	return false
}

// dangerousBashPatterns 是匹配破坏性命令的 glob 模式列表。
// 仅用于 UI 警告提示 — deny 列表才是实际的执行拦截机制。
var dangerousBashPatterns = []struct {
	pattern string
	label   string
}{
	{"rm -rf*", "recursive delete"},
	{"rm -r *", "recursive delete"},
	{"rm -fr*", "recursive delete"},
	{"git push*--force*", "force push"},
	{"git push*-f*", "force push"},
	{"git reset --hard*", "hard reset"},
	{"git clean -f*", "force clean"},
	{"chmod 777*", "world-writable"},
	{"chmod -R 777*", "world-writable recursive"},
	{"chown *", "ownership change"},
	{"sudo *", "superuser"},
	{"mkfs*", "filesystem format"},
	{"dd if=*", "raw device write"},
	{"fdisk*", "partition table"},
	{"> /dev/*", "device overwrite"},
}

// BashDangerWarning 如果命令匹配已知的危险模式则返回简短标签，否则返回 ""。
// 这只是一个视觉提示 — Policy 规则才是真正的权限判断依据。
func BashDangerWarning(subject string) string {
	s := strings.TrimSpace(subject)
	for _, d := range dangerousBashPatterns {
		if matchGlob(d.pattern, s) {
			return d.label
		}
	}
	return ""
}
