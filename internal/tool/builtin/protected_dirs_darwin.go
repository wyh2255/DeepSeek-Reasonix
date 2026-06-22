// protected_dirs_darwin.go 定义 macOS 平台受 TCC（透明度、同意和控制）保护的用户目录。
package builtin

import (
	"os"
	"path/filepath"
)

// protectedDirs 是 macOS TCC 保护的用户目录集合。
// 打开这些目录会触发隐私同意提示（"想要访问 Apple Music / 媒体库"），
// 因此递归 glob/grep 会跳过它们。按绝对路径匹配（非基名），
// 所以项目中同名目录（如 ~/Projects/Music）是安全的。
var protectedDirs = func() map[string]bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	m := make(map[string]bool, 4)
	for _, d := range []string{"Music", "Pictures", "Movies", "Library"} {
		m[filepath.Clean(filepath.Join(home, d))] = true
	}
	return m
}()

func isProtectedDir(abs string) bool { return protectedDirs[abs] }
