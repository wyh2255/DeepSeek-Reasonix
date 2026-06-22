// upgrade.go 实现了 CLI 的自动升级命令（reasonix upgrade / reasonix update）。
// 功能流程：从 GitHub Releases API 获取最新版本 -> 与当前版本比较 ->
// 下载对应平台的归档文件 -> SHA256 校验 -> 解压二进制 -> 原子替换当前可执行文件。
// 支持 .tar.gz（Linux/macOS）和 .zip（Windows）两种归档格式。
package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
	"reasonix/internal/netclient"

	"golang.org/x/mod/semver"
)

const (
	ghOwner        = "esengine"                                                    // GitHub 仓库所有者
	ghRepo         = "DeepSeek-Reasonix"                                           // GitHub 仓库名
	ghAPIReleases  = "https://api.github.com/repos/" + ghOwner + "/" + ghRepo + "/releases"   // Releases API 地址
	ghDownloadBase = "https://github.com/" + ghOwner + "/" + ghRepo + "/releases/download"    // 下载基础 URL
	upgradeTimeout = 60 * time.Second                                              // HTTP 请求超时时间
)

// ghRelease 是 GitHub Release API 响应的子集，仅包含标签名和资源列表。
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []ghAsset
}

// ghAsset 表示一个 Release 资源文件（如 reasonix-linux-amd64.tar.gz）。
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// upgradeCommand 处理 "reasonix upgrade"（及 "reasonix update"）命令。
// 支持 --check（仅检查不安装）和 --force（强制重装当前版本）参数。
// 返回 0 表示成功，1 表示错误，2 表示参数解析失败。
func upgradeCommand(args []string, version string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "check for updates without installing")
	force := fs.Bool("force", false, "reinstall even if already on the latest version")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 1. Normalize running version.
	cur, ok := normalizeVersion(version)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s %s\n", i18n.M.ErrorPrefix, i18n.M.UpgradeDevBuild)
		return 1
	}

	// 2. Build HTTP client using configured proxy.
	cfg, _ := config.Load()
	spec := cfg.NetworkProxySpec()
	c, err := netclient.NewHTTPClient(spec, netclient.TransportOptions{
		ResponseHeaderTimeout: upgradeTimeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", i18n.M.ErrorPrefix, err)
		return 1
	}

	// 3. Fetch latest release from GitHub API.
	fmt.Println(i18n.M.UpgradeChecking)
	rel, err := fetchLatestRelease(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s "+i18n.M.UpgradeFetchFailed+"\n", i18n.M.ErrorPrefix, err)
		return 1
	}

	// 4. Compare versions.
	latest := rel.TagName
	if !strings.HasPrefix(latest, "v") {
		latest = "v" + latest
	}
	if !semver.IsValid(latest) {
		fmt.Fprintf(os.Stderr, "%s "+i18n.M.UpgradeInvalidVersion+"\n", i18n.M.ErrorPrefix, latest)
		return 1
	}
	if semver.Compare(latest, cur) <= 0 {
		if *force {
			fmt.Println(i18n.M.UpgradeForcing)
		} else {
			fmt.Println(i18n.M.UpgradeAlreadyLatest)
			return 0
		}
	} else {
		fmt.Printf(i18n.M.UpgradeAvailableFmt+"\n", cur, latest)
	}

	if *checkOnly {
		return 0
	}

	// 5. Find the asset for the current platform.
	base := fmt.Sprintf("reasonix-%s-%s", runtime.GOOS, runtime.GOARCH)
	var asset *ghAsset
	for i := range rel.Assets {
		if strings.HasPrefix(rel.Assets[i].Name, base) {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil {
		fmt.Fprintf(os.Stderr, "%s "+i18n.M.UpgradeNoAssetFmt+"\n", i18n.M.ErrorPrefix, base)
		return 1
	}

	// 6. Find the checksum URL.
	checksumURL := fmt.Sprintf("%s/%s/SHA256SUMS", ghDownloadBase, rel.TagName)

	// 7. Download archive.
	fmt.Printf(i18n.M.UpgradeDownloadingFmt+"\n", asset.Name, humanSize(asset.Size))
	archiveData, err := fetchBytes(c, asset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s "+i18n.M.UpgradeDownloadFailed+"\n", i18n.M.ErrorPrefix, err)
		return 1
	}

	// 8. Verify SHA256 checksum — fail closed: abort on any verification error.
	fmt.Println(i18n.M.UpgradeVerifying)
	checksumData, err := fetchBytes(c, checksumURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s "+i18n.M.UpgradeChecksumFailed+"\n", i18n.M.ErrorPrefix, err)
		return 1
	}
	if err := verifyChecksum(archiveData, asset.Name, checksumData); err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", i18n.M.ErrorPrefix, err)
		return 1
	}

	// 9. Extract binary from archive.
	binName := "reasonix"
	if runtime.GOOS == "windows" {
		binName = "reasonix.exe"
	}
	binary, err := extractBinary(archiveData, asset.Name, binName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s "+i18n.M.UpgradeExtractFailed+"\n", i18n.M.ErrorPrefix, err)
		return 1
	}

	// 10. Replace the running binary.
	fmt.Println(i18n.M.UpgradeApplying)
	if err := replaceBinary(binary); err != nil {
		fmt.Fprintf(os.Stderr, "%s "+i18n.M.UpgradeApplyFailed+"\n", i18n.M.ErrorPrefix, err)
		return 1
	}

	fmt.Printf(i18n.M.UpgradeSuccessFmt+"\n", latest)
	return 0
}

// normalizeVersion 将版本字符串规范化为 semver 格式（"vX.Y.Z"）。
// 空字符串或 "dev" 视为开发版本，返回 ok=false。
func normalizeVersion(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return "", false
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return "", false
	}
	return semver.Canonical(v), true
}

// isCLITag 判断标签是否属于 CLI 发布命名空间（v* 开头后跟数字）。
// 排除 "desktop-v1.5.0"、"npm-v1.4.0" 等其他命名空间的标签。
func isCLITag(tag string) bool {
	tag = strings.TrimSpace(tag)
	return len(tag) >= 2 && tag[0] == 'v' && tag[1] >= '0' && tag[1] <= '9'
}

// pickCLIRelease 从按时间倒序排列的 Release 列表中选取最新的 CLI 命名空间（v*）Release。
// 跳过 "desktop-v"、"npm-v" 等其他命名空间。保留预发布版本，因为 1.x 系列
// 通过 npm @next 发布为 rc 版，没有稳定版用户需要顾虑。
func pickCLIRelease(rels []ghRelease) *ghRelease {
	for i := range rels {
		if isCLITag(rels[i].TagName) {
			return &rels[i]
		}
	}
	return nil
}

// fetchLatestRelease 查询 GitHub Releases API，返回最新的 CLI 命名空间 Release。
func fetchLatestRelease(c *http.Client) (*ghRelease, error) {
	req, err := http.NewRequest("GET", ghAPIReleases, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "reasonix-cli")

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API: %s", resp.Status)
	}

	var rels []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}

	if rel := pickCLIRelease(rels); rel != nil {
		return rel, nil
	}
	return nil, fmt.Errorf("no CLI release (v*) found in recent releases")
}

// fetchBytes 通过 HTTP GET 请求将 URL 内容完整下载到内存中。
func fetchBytes(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// verifyChecksum 校验下载数据的 SHA256 哈希值是否与 SHA256SUMS 文件中对应条目匹配。
// 不匹配则返回错误，条目不存在也返回错误（fail-closed 策略）。
func verifyChecksum(data []byte, fileName string, checksumFile []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])

	for _, line := range strings.Split(strings.TrimSpace(string(checksumFile)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == fileName {
			if !strings.EqualFold(parts[0], got) {
				return fmt.Errorf(i18n.M.UpgradeChecksumMismatchFmt, got, parts[0])
			}
			return nil
		}
	}
	return fmt.Errorf(i18n.M.UpgradeChecksumNotFoundFmt, fileName)
}

// extractBinary 根据归档文件扩展名选择解压方式，从 .tar.gz 或 .zip 中提取指定的二进制文件。
func extractBinary(data []byte, archiveName, binaryName string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractFromZip(data, binaryName)
	}
	return extractFromTarGz(data, binaryName)
}

// extractFromTarGz 从 .tar.gz 归档中解压指定名称的二进制文件。
func extractFromTarGz(data []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && (h.Name == name || strings.HasSuffix(h.Name, "/"+name)) {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%q not found in archive", name)
}

// extractFromZip 从 .zip 归档中解压指定名称的二进制文件（主要用于 Windows 平台）。
func extractFromZip(data []byte, name string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		if base == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%q not found in zip archive", name)
}

// replaceBinary writes newBin to the running executable's path atomically.
//
// On Unix this is a simple temp-file + rename. On Windows the running
// executable is memory-mapped and cannot be overwritten directly, so we
// rename it aside to .reasonix.old first, then place the new binary.
// The .old file is cleaned up best-effort (Windows may still hold a lock
// on it; we hide it in that case).
func replaceBinary(newBin []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	resolved, err := resolveSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	dir := filepath.Dir(resolved)
	base := filepath.Base(resolved)
	tmpPath := filepath.Join(dir, fmt.Sprintf(".%s.new", base))

	// Write new binary to .new temp file.
	if err := os.WriteFile(tmpPath, newBin, 0o755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}

	if runtime.GOOS == "windows" {
		return commitWindows(resolved, tmpPath, base, dir)
	}

	// Unix: atomic rename .new → target.
	if err := os.Rename(tmpPath, resolved); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// commitWindows 在 Windows 上执行两阶段替换：
//  1. 将正在运行的 exe 重命名为 .old（运行中允许重命名）
//  2. 将 .new 重命名为目标文件
//  3. 尝试删除 .old（若仍被锁定则隐藏该文件）
func commitWindows(target, newPath, base, dir string) error {
	oldPath := filepath.Join(dir, fmt.Sprintf(".%s.old", base))

	// Remove any leftover .old from a previous update.
	_ = os.Remove(oldPath)

	// Move the running executable aside.
	if err := os.Rename(target, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("rename running exe aside: %w", err)
	}

	// Move the new binary into place.
	if err := os.Rename(newPath, target); err != nil {
		// Rollback: try to restore the old binary.
		if rerr := os.Rename(oldPath, target); rerr != nil {
			return fmt.Errorf("replace failed (%v); rollback also failed: %w", err, rerr)
		}
		return fmt.Errorf("rename new binary: %w", err)
	}

	// Best-effort cleanup of the old binary.
	if err := os.Remove(oldPath); err != nil {
		// Windows may hold a lock; hide the file so it doesn't clutter the dir.
		hideFileWindows(oldPath)
	}
	return nil
}

// resolveSymlinks 解析符号链接，失败时回退返回原始路径。
func resolveSymlinks(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p, nil
	}
	return r, nil
}

// humanSize 将字节数转换为人类可读的大小格式（B / KiB / MiB）。
func humanSize(b int64) string {
	const (
		_KiB = 1024
		_MiB = 1024 * _KiB
	)
	switch {
	case b >= _MiB:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(_MiB))
	case b >= _KiB:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(_KiB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
