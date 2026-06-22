//go:build !darwin

// protected_dirs_other.go 为非 macOS 平台提供空的受保护目录实现。
// 非 macOS 平台没有 TCC 保护机制，所以所有目录都不受保护。
package builtin

// isProtectedDir 在非 macOS 平台上始终返回 false。
func isProtectedDir(string) bool { return false }
