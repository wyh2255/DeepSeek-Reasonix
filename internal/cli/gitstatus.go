// gitstatus.go 实现了 Git 仓库状态的异步获取与终端显示。
// 该文件负责：
//   - 通过 git 命令行工具异步获取当前仓库的分支、提交差异和未跟踪文件信息
//   - 将 Git 状态渲染为带颜色的终端文本（如 repo@branch (+3 -1 ?2)）
//   - 在终端宽度受限时对仓库名和分支名进行智能截断
//   - 根据当前模式（自动/计划/yolo/shell）显示不同的状态颜色
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// gitStatusTimeout 是获取 Git 状态的超时时间，避免在大型仓库中阻塞 UI
const gitStatusTimeout = 700 * time.Millisecond

// gitStatus 表示一个 Git 仓库的状态快照，包含仓库名、分支信息和文件变更统计
type gitStatus struct {
	Repo      string
	Branch    string
	Detached  bool
	Added     int
	Removed   int
	Untracked int // 未跟踪文件数量
}

// fetchGitStatus 返回一个 tea.Cmd，用于异步获取 Git 状态。
// 该命令在后台 goroutine 中执行，通过 context 控制超时，
// 成功时返回 gitStatusMsg，失败时返回空的 gitStatusMsg。
func fetchGitStatus() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), gitStatusTimeout)
		defer cancel()
		status, err := loadGitStatus(ctx, "")
		if err != nil {
			return gitStatusMsg{}
		}
		return gitStatusMsg{status: status}
	}
}

// loadGitStatus 执行实际的 Git 命令来加载仓库状态。
// 它依次获取仓库根目录、当前分支、文件变更统计和未跟踪文件数量。
// cwd 为空时使用当前工作目录；ctx 用于控制命令超时。
func loadGitStatus(ctx context.Context, cwd string) (gitStatus, error) {
	root, err := runGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitStatus{}, err
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return gitStatus{}, errors.New("empty git root")
	}

	status := gitStatus{Repo: filepath.Base(root)}
	if branch, err := runGit(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && strings.TrimSpace(branch) != "" {
		status.Branch = strings.TrimSpace(branch)
	} else if sha, err := runGit(ctx, root, "rev-parse", "--short", "HEAD"); err == nil && strings.TrimSpace(sha) != "" {
		status.Branch = strings.TrimSpace(sha)
		status.Detached = true
	} else if ref, err := runGit(ctx, root, "symbolic-ref", "--short", "HEAD"); err == nil && strings.TrimSpace(ref) != "" {
		status.Branch = strings.TrimSpace(ref)
	}
	if status.Branch == "" {
		status.Branch = "HEAD"
		status.Detached = true
	}

	if out, err := runGit(ctx, root, "diff", "--numstat", "HEAD", "--"); err == nil {
		status.Added, status.Removed = parseGitNumstat(out)
	}
	if out, err := runGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=normal"); err == nil {
		status.Untracked = countUntracked(out)
	}
	return status, nil
}

// runGit 执行一个 git 命令并返回其标准输出。
// 设置 GIT_OPTIONAL_LOCKS=0 以避免获取不必要的锁，提高并发安全性。
func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseGitNumstat 解析 git diff --numstat 的输出，累计所有文件的新增和删除行数。
// 输出格式为每行 "added\tremoved\tfilename"，二进制文件显示为 "-"。
func parseGitNumstat(out string) (added int, removed int) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != "-" {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				added += n
			}
		}
		if fields[1] != "-" {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				removed += n
			}
		}
	}
	return added, removed
}

// countUntracked 统计 git status --porcelain 输出中以 "??" 开头的行数，
// 即未跟踪文件的数量。
func countUntracked(out string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "?? ") {
			n++
		}
	}
	return n
}

// gitTag 返回当前 Git 状态的终端显示标签（如 "repo@branch"）。
// 当仓库名或分支名为空时返回空字符串。
func (m chatTUI) gitTag() string {
	if strings.TrimSpace(m.gitStatus.Repo) == "" || strings.TrimSpace(m.gitStatus.Branch) == "" {
		return ""
	}
	return m.gitStatus.render(themeFg(m.statusModeColor(), m.gitStatus.Repo), m.gitStatus.Branch)
}

// 各种模式下状态栏的颜色配置
var (
	statusAutoColor  = cliColor{"#f59e0b", 214} // 自动模式：琥珀色
	statusPlanColor  = cliColor{"#2563eb", 27}   // 计划模式：蓝色
	statusYoloColor  = cliColor{"#e5484d", 167}  // YOLO模式：红色
	statusShellColor = cliColor{"#16a34a", 71}   // Shell模式：绿色
)

// statusModeColor 返回当前操作模式对应的状态栏颜色。
// YOLO模式（自动批准工具）为红色，计划模式为蓝色，默认为琥珀色。
func (m chatTUI) statusModeColor() cliColor {
	switch {
	case m.ctrl != nil && m.ctrl.AutoApproveTools():
		return statusYoloColor
	case m.planMode:
		return statusPlanColor
	default:
		return statusAutoColor
	}
}

// Render 将 Git 状态渲染为带颜色的终端字符串，使用默认的强调色显示仓库名。
func (s gitStatus) Render() string {
	return s.RenderRepo(accent(s.Repo))
}

// RenderRepo 使用指定的仓库名字符串渲染 Git 状态。
func (s gitStatus) RenderRepo(repo string) string {
	if strings.TrimSpace(s.Repo) == "" || strings.TrimSpace(s.Branch) == "" {
		return ""
	}
	return s.render(repo, s.Branch)
}

// RenderWithin 在指定的最大宽度内渲染 Git 状态，自动截断过长的文本。
// 当宽度不足时，会智能压缩仓库名和分支名的显示。
func (s gitStatus) RenderWithin(maxWidth int, repoColor cliColor) string {
	if strings.TrimSpace(s.Repo) == "" || strings.TrimSpace(s.Branch) == "" {
		return ""
	}
	repo, branch := s.compactIdentity(maxWidth)
	out := s.render(themeFg(repoColor, repo), branch)
	if maxWidth > 0 && visibleWidth(out) > maxWidth {
		return ansi.Truncate(out, maxWidth, "…")
	}
	return out
}

// compactIdentity 根据可用宽度智能压缩仓库名和分支名。
// 优先保留分支名的可读性，必要时对仓库名进行中间截断。
func (s gitStatus) compactIdentity(maxWidth int) (repo, branch string) {
	repo = strings.TrimSpace(s.Repo)
	branch = strings.TrimSpace(s.Branch)
	if maxWidth <= 0 {
		return repo, branch
	}
	dirtyWidth := visibleWidth(s.dirtyPlain())
	nameBudget := maxWidth - dirtyWidth - visibleWidth("@")
	if nameBudget <= 2 {
		return compactEnd(repo, max(1, nameBudget)), ""
	}
	repoWidth := visibleWidth(repo)
	branchWidth := visibleWidth(branch)
	if repoWidth+branchWidth <= nameBudget {
		return repo, branch
	}

	minRepo := min(repoWidth, 8)
	if repoBudget := nameBudget - branchWidth; repoBudget >= minRepo {
		return compactMiddle(repo, repoBudget), branch
	}

	repoBudget := min(repoWidth, max(4, min(10, nameBudget/3)))
	if nameBudget-repoBudget < 8 {
		repoBudget = max(1, nameBudget-8)
	}
	branchBudget := max(1, nameBudget-repoBudget)
	return compactMiddle(repo, repoBudget), compactMiddle(branch, branchBudget)
}

// dirtyPlain 返回不含颜色代码的变更统计文本（如 " (+3 -1 ?2)"），用于宽度计算。
func (s gitStatus) dirtyPlain() string {
	var parts []string
	if s.Added > 0 || s.Removed > 0 {
		parts = append(parts, fmt.Sprintf("+%d", s.Added), fmt.Sprintf("-%d", s.Removed))
	}
	if s.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("?%d", s.Untracked))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

// render 将 Git 状态渲染为带 ANSI 颜色的终端字符串。
// 格式为 "repo@branch (+added -removed ?untracked)"，
// 分离 HEAD 时分支名显示为黄色，否则为绿色。
func (s gitStatus) render(repo, branch string) string {
	var b strings.Builder
	b.WriteString(repo)
	b.WriteString(dim("@"))
	if s.Detached {
		b.WriteString(yellow(branch))
	} else {
		b.WriteString(green(branch))
	}

	var parts []string
	if s.Added > 0 || s.Removed > 0 {
		parts = append(parts, green(fmt.Sprintf("+%d", s.Added)), red(fmt.Sprintf("-%d", s.Removed)))
	}
	if s.Untracked > 0 {
		parts = append(parts, yellow(fmt.Sprintf("?%d", s.Untracked)))
	}
	if len(parts) > 0 {
		b.WriteString(dim(" ("))
		b.WriteString(strings.Join(parts, " "))
		b.WriteString(dim(")"))
	}
	return b.String()
}
