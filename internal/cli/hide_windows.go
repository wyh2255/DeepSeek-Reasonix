//go:build windows

// hide_windows.go 为 Windows 平台提供文件隐藏功能。
// 自动更新后会残留 .old 旧二进制文件，此文件通过设置 Windows 文件隐藏属性
// 使这些文件在资源管理器中默认不可见，保持目录整洁。
package cli

import "syscall"

// hideFileWindows 设置指定路径文件的 FILE_ATTRIBUTE_HIDDEN 属性，
// 使自动更新后残留的 .old 旧二进制文件在资源管理器中不可见。
// 这是尽力而为的操作，错误会被静默忽略。
func hideFileWindows(path string) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	// FILE_ATTRIBUTE_HIDDEN = 0x02
	syscall.SetFileAttributes(p, 0x02)
}
