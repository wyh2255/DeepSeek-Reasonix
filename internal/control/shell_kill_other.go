// 文件：shell_kill_other.go
//
// Unix 平台的 shell 进程树终止实现。
// 当取消发生时，杀死整个 shell 进程树（通过 SIGKILL 向负 PID 发送信号）。
// 将子进程放在新会话中也可防止交互式提示抓取 TUI 的控制终端。

//go:build !windows

package control

import (
	"os/exec"
	"syscall"
)

// setShellKillTree makes cancellation kill the whole shell tree. Running the
// child in a new session also keeps interactive prompts from grabbing the TUI's
// controlling terminal.
func setShellKillTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
