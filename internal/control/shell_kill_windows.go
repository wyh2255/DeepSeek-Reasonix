// 文件：shell_kill_windows.go
//
// Windows 平台的 shell 进程树终止实现。
// Windows 不会将 kill 级联到子进程，因此杀死 shell 后生成的命令仍在运行；
// taskkill /T 遍历 PID 树，/F 强制终止。同时隐藏子进程的控制台窗口。

//go:build windows

package control

import (
	"os/exec"
	"strconv"

	"reasonix/internal/proc"
)

// setShellKillTree hides the child's console and makes a cancelled command kill
// its whole process tree. Windows does not cascade a kill to child processes, so
// killing the shell leaves spawned commands running after a timeout; taskkill /T
// walks the PID tree and /F forces it.
func setShellKillTree(cmd *exec.Cmd) {
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
