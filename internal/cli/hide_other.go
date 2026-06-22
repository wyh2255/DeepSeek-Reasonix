//go:build !windows

// hide_other.go 为非 Windows 平台提供 hideFileWindows 的空实现。
// 在 Windows 上该函数用于设置文件的隐藏属性，但在其他平台上不需要此功能，
// 因此提供一个无操作的存根实现。
package cli

// hideFileWindows 在非 Windows 平台上为空操作（no-op）。
// 该函数的存在是为了跨平台编译兼容性，实际的文件隐藏逻辑仅在 Windows 构建中实现。
func hideFileWindows(_ string) {}
