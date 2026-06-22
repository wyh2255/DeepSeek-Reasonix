//go:build !windows

// bash_kill_other.go 提供非 Windows 平台（Linux/macOS）的进程组终止实现。
package builtin

import (
	"os/exec"
	"syscall"
)

// setKillTree 使被取消的命令终止其整个进程组，而不仅仅是 shell 领导进程。
// 否则 "go test ./..." 及其生成的测试二进制文件会在用户按 Esc 后继续运行。
// 子进程获得独立的进程组（Setpgid），使负 PID 信号能到达所有后代进程。
func setKillTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// reapTree 终止命令返回后仍在运行的进程组成员。
// 前台命令 fork 出守护进程（如 "bazel run" 的服务器）时会留下孤儿进程，
// Wait 只回收了 shell 领导进程。进程组 ID 就是领导进程的 PID（Setpgid）。
// ESRCH（空组）是正常的。参见 #3702。
func reapTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
