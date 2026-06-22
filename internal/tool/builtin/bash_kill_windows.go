// bash_kill_windows.go 提供 Windows 平台的进程树终止实现。
package builtin

import (
	"os/exec"
	"strconv"

	"reasonix/internal/proc"
)

// setKillTree 隐藏子进程控制台并使命令取消时终止整个进程树。
// Windows 不会级联终止子进程，所以仅终止 shell 会让 "go test" 及其生成的
// 二进制文件在用户按 Esc 后继续运行；taskkill /T 遍历 PID 树，/F 强制终止。
func setKillTree(cmd *exec.Cmd) {
	proc.HideWindow(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		proc.HideWindow(kill)
		_ = kill.Run()
		return cmd.Process.Kill()
	}
}

// reapTree 是 POSIX 构建中 #3702 的完成后进程组清理。
// 在 Windows 上是空操作：shell 领导进程退出后没有活着的父进程供 taskkill /T 遍历，
// 每次 bash 调用都生成 taskkill 会拖累热路径且收益不大 — 真正的退出后树清理
// 需要 Job Object，这超出了当前范围。
func reapTree(*exec.Cmd) {}
