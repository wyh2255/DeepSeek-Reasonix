// language.go 实现了 /language 命令，用于查看和切换界面语言。
// 该文件负责：
//   - 解析语言参数（auto/en/zh）
//   - 读取和写入配置文件中的语言设置
//   - 清理用户级配置中的语言覆盖（当设置为 auto 时）
//   - 显示当前语言状态和可用选项
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
)

// runLanguageSubcommand 处理 /language 命令的执行。
// 无参数时显示当前语言设置和可用选项；
// 带参数时将语言设置保存到配置文件，并清除用户级的语言覆盖。
func (m *chatTUI) runLanguageSubcommand(input string) {
	args := tokenizeArgs(input)
	if len(args) < 2 {
		cfg, err := config.Load()
		if err != nil {
			m.notice("language: " + err.Error())
			return
		}
		saved := languageDisplay(cfg.Language)
		resolved := i18n.DetectLanguage(cfg.Language)
		m.notice(i18n.M.LanguageHeader + "\n" + describeLanguages(saved, resolved) + "\n" + i18n.M.LanguageHint)
		return
	}
	if len(args) > 2 {
		m.notice(i18n.M.LanguageHint)
		return
	}

	lang, err := normalizeLanguageArg(args[1])
	if err != nil {
		m.notice(err.Error())
		return
	}
	path := config.SourcePath()
	if path == "" {
		path = config.UserConfigPath()
	}
	if path == "" {
		m.notice("language: cannot resolve config path")
		return
	}
	edit := config.LoadForEdit(path)
	if err := edit.SetLanguage(lang); err != nil {
		m.notice("language: " + err.Error())
		return
	}
	if err := edit.SaveTo(path); err != nil {
		m.notice("language: " + err.Error())
		return
	}
	if lang == "" {
		if err := clearUserLanguageOverride(path); err != nil {
			m.notice("language: " + err.Error())
			return
		}
	}

	resolved := i18n.DetectLanguage(lang)
	m.notice(fmt.Sprintf(i18n.M.LanguageChangedFmt, languageDisplay(lang), resolved))
}

// clearUserLanguageOverride 清除用户级配置文件中的语言覆盖设置。
// 当语言被设为 auto 时调用，确保不会残留之前的语言偏好。
func clearUserLanguageOverride(primaryPath string) error {
	userPath := config.UserConfigPath()
	if userPath == "" || sameConfigPath(primaryPath, userPath) {
		return nil
	}
	if _, err := os.Stat(userPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	edit := config.LoadForEdit(userPath)
	if strings.TrimSpace(edit.Language) == "" {
		return nil
	}
	if err := edit.SetLanguage(""); err != nil {
		return err
	}
	return edit.SaveTo(userPath)
}

// sameConfigPath 比较两个配置文件路径是否指向同一个文件。
// 先转换为绝对路径再进行比较，避免相对路径导致的误判。
func sameConfigPath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil {
		a = aa
	}
	if errB == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// normalizeLanguageArg 将用户输入的语言参数标准化为内部语言代码。
// 支持 "auto"/"detect"/"default"（返回空字符串表示自动检测）、
// "en"/"english"、"zh"/"cn"/"chinese"/"中文"。
func normalizeLanguageArg(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto", "detect", "default":
		return "", nil
	case "en", "english":
		return "en", nil
	case "zh", "cn", "chinese", "中文":
		return "zh", nil
	default:
		return "", fmt.Errorf("usage: /language auto|en|zh")
	}
}

// languageDisplay 返回语言代码的显示文本，空字符串显示为 "auto"。
func languageDisplay(lang string) string {
	if strings.TrimSpace(lang) == "" {
		return "auto"
	}
	return lang
}

// describeLanguages 生成语言选项的描述列表，标记当前选中的语言，
// 并在 auto 模式下显示实际解析出的语言。
func describeLanguages(current, resolved string) string {
	items := []struct {
		tag  string
		hint string
	}{
		{"auto", i18n.M.ArgLanguageAuto},
		{"en", i18n.M.ArgLanguageEn},
		{"zh", i18n.M.ArgLanguageZh},
	}
	var b strings.Builder
	for _, it := range items {
		marker := "  "
		if it.tag == current {
			marker = "• "
		}
		hint := it.hint
		if it.tag == current {
			hint += " · " + i18n.M.ArgThemeCurrent
		}
		if it.tag == "auto" && current == "auto" {
			hint += " · " + resolved
		}
		fmt.Fprintf(&b, "%s%-6s %s\n", marker, it.tag, hint)
	}
	return strings.TrimRight(b.String(), "\n")
}
