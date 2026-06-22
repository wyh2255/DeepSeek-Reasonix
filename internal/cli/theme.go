// theme.go 实现了 CLI 的主题系统。
// 负责管理深色/浅色主题的配色方案、主题样式变体（如 graphite、aurora 等）、
// 终端背景色自动检测（OSC 11 协议与 COLORFGBG 环境变量回退）、
// 以及所有 UI 组件（输入框、滚动条、状态栏等）的样式刷新。
package cli

import (
	"fmt"
	"image/color"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
)

// cliColor 表示一个颜色值，同时保存十六进制字符串和 xterm 256 色编号，
// 以便在支持真彩色的终端使用 hex，在不支持时回退到 xterm。
type cliColor struct {
	hex   string
	xterm int
}

// cliPalette 是一个完整的主题配色方案，包含所有 UI 元素所需的颜色定义。
type cliPalette struct {
	name         string
	style        string
	accent       cliColor
	muted        cliColor
	faint        cliColor
	success      cliColor
	warn         cliColor
	err          cliColor
	danger       cliColor
	border       cliColor
	selection    cliColor
	userBubbleBG cliColor
	diffAddBG    cliColor
	diffDelBG    cliColor
	toolRead     cliColor
	toolProc     cliColor
}

// cliThemeStyle 定义一个主题样式变体（如 graphite、aurora），包含样式名称、
// 所属的明暗模式、强调色和描述信息。
type cliThemeStyle struct {
	name        string
	mode        string
	accent      cliColor
	description string
}

// 预定义的深色和浅色基础主题，以及所有可用的样式变体列表。
var (
	cliDarkTheme = cliPalette{
		name:         "dark",
		style:        "graphite",
		accent:       cliColor{"#d97757", 173},
		muted:        cliColor{"#c0c4cc", 251},
		faint:        cliColor{"#858b96", 245},
		success:      cliColor{"#74b87a", 108},
		warn:         cliColor{"#d9a441", 179},
		err:          cliColor{"#e0696a", 167},
		danger:       cliColor{"#e5484d", 167},
		border:       cliColor{"#343945", 237},
		selection:    cliColor{"#d97757", 173},
		userBubbleBG: cliColor{"#222631", 235},
		diffAddBG:    cliColor{"#14351d", 22},
		diffDelBG:    cliColor{"#3a1619", 52},
		toolRead:     cliColor{"#56b6c2", 80},
		toolProc:     cliColor{"#c678dd", 176},
	}
	cliLightTheme = cliPalette{
		name:         "light",
		style:        "sandstone",
		accent:       cliColor{"#2f5fa8", 25},
		muted:        cliColor{"#555049", 239},
		faint:        cliColor{"#82796f", 243},
		success:      cliColor{"#5d9b66", 65},
		warn:         cliColor{"#b68120", 136},
		err:          cliColor{"#b94b4d", 131},
		danger:       cliColor{"#e5484d", 167},
		border:       cliColor{"#ded4c6", 252},
		selection:    cliColor{"#6f91d9", 68},
		userBubbleBG: cliColor{"#f5f0e8", 255},
		diffAddBG:    cliColor{"#e5f3e7", 254},
		diffDelBG:    cliColor{"#fae8e8", 255},
		toolRead:     cliColor{"#6f91d9", 68},
		toolProc:     cliColor{"#8a6bb8", 97},
	}
	cliThemeStyles = []cliThemeStyle{
		{name: "graphite", mode: "dark", accent: cliColor{"#d97757", 173}, description: "warm clay accent"},
		{name: "ember", mode: "dark", accent: cliColor{"#f06d38", 209}, description: "hot orange accent"},
		{name: "aurora", mode: "dark", accent: cliColor{"#34c3a6", 79}, description: "cool teal accent"},
		{name: "midnight", mode: "dark", accent: cliColor{"#b18cff", 141}, description: "quiet violet accent"},
		{name: "sandstone", mode: "light", accent: cliColor{"#c2613f", 173}, description: "default warm light accent"},
		{name: "porcelain", mode: "light", accent: cliColor{"#7d63c8", 104}, description: "soft violet light accent"},
		{name: "linen", mode: "light", accent: cliColor{"#bd5d4d", 167}, description: "muted coral light accent"},
		{name: "glacier", mode: "light", accent: cliColor{"#357fa8", 74}, description: "cool blue light accent"},
	}
	activeCLITheme                  = applyCLIThemeStyle(cliDarkTheme, cliThemeStyles[0])
	queryTerminalBackgroundForTheme = queryTerminalBackground
)

// configureCLITheme 根据给定的明暗模式（"dark"/"light"/"auto"）配置 CLI 主题。
func configureCLITheme(mode string) {
	configureCLIThemeWithStyle(mode, "")
}

// configureCLIThemeWithStyle 根据明暗模式和样式名称配置 CLI 主题。
// 优先读取 REASONIX_THEME 和 REASONIX_THEME_STYLE 环境变量覆盖参数值。
func configureCLIThemeWithStyle(mode, style string) {
	if env := strings.TrimSpace(os.Getenv("REASONIX_THEME")); env != "" {
		if st, ok := cliThemeStyleByName(env); ok {
			mode = st.mode
			style = st.name
		} else {
			mode = env
		}
	}
	if env := strings.TrimSpace(os.Getenv("REASONIX_THEME_STYLE")); env != "" {
		style = env
	}
	activeCLITheme = resolveCLIThemeWithStyle(mode, style)
	refreshCLIStyles()
}

// resolveCLITheme 根据明暗模式解析并返回最终的主题配色方案。
func resolveCLITheme(mode string) cliPalette {
	return resolveCLIThemeWithStyle(mode, "")
}

// resolveCLIThemeWithStyle 根据明暗模式和样式名称解析主题。
// 如果 mode 本身是一个样式名（如 "aurora"），则直接使用该样式；
// 否则根据 mode 确定明暗模式，再查找匹配的样式。
func resolveCLIThemeWithStyle(mode, style string) cliPalette {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if st, ok := cliThemeStyleByName(mode); ok {
		return buildCLITheme(st.mode, st.name)
	}
	resolvedMode := resolveCLIThemeMode(mode)
	st, ok := cliThemeStyleByName(style)
	if !ok || st.mode != resolvedMode {
		st = defaultCLIThemeStyle(resolvedMode)
	}
	return buildCLITheme(resolvedMode, st.name)
}

// resolveCLIThemeMode 将用户指定的模式字符串解析为 "dark" 或 "light"。
// 当模式为 "auto" 或空时，依次尝试：OSC 11 终端背景色检测、COLORFGBG 环境变量，
// 最终回退到 "dark"。
func resolveCLIThemeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "light":
		return "light"
	case "dark":
		return "dark"
	case "auto", "":
		if rgb, ok := queryTerminalBackgroundForTheme(); ok {
			if rgb.looksLight() {
				return "light"
			}
			return "dark"
		}
		if colorFGBGLooksLight() {
			return "light"
		}
		return "dark"
	default:
		return "dark"
	}
}

// buildCLITheme 根据明暗模式和样式名称构建最终的 cliPalette。
// 先选择深色或浅色基础主题，再应用样式变体的强调色。
func buildCLITheme(mode, style string) cliPalette {
	base := cliDarkTheme
	if mode == "light" {
		base = cliLightTheme
	}
	st, ok := cliThemeStyleByName(style)
	if !ok || st.mode != base.name {
		st = defaultCLIThemeStyle(base.name)
	}
	return applyCLIThemeStyle(base, st)
}

// applyCLIThemeStyle 将样式变体的强调色应用到基础主题上，返回修改后的新主题。
func applyCLIThemeStyle(base cliPalette, style cliThemeStyle) cliPalette {
	base.style = style.name
	base.accent = style.accent
	base.selection = style.accent
	return base
}

// cliThemeStyleByName 根据名称查找预定义的样式变体，返回样式和是否找到。
func cliThemeStyleByName(name string) (cliThemeStyle, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, st := range cliThemeStyles {
		if st.name == name {
			return st, true
		}
	}
	return cliThemeStyle{}, false
}

// defaultCLIThemeStyle 返回指定明暗模式下的默认样式：浅色模式默认 "sandstone"，深色默认 "graphite"。
func defaultCLIThemeStyle(mode string) cliThemeStyle {
	if mode == "light" {
		for _, st := range cliThemeStyles {
			if st.name == "sandstone" {
				return st
			}
		}
	}
	return cliThemeStyles[0]
}

// withoutTerminalProbe resolves a theme with the OSC background probe disabled —
// for callers running while something else (the live TUI) owns stdin, where a
// raw-mode read would fight the TUI's input reader. "auto" then falls back to the
// COLORFGBG heuristic.
func withoutTerminalProbe(fn func()) {
	prev := queryTerminalBackgroundForTheme
	queryTerminalBackgroundForTheme = func() (terminalRGB, bool) { return terminalRGB{}, false }
	defer func() { queryTerminalBackgroundForTheme = prev }()
	fn()
}

// setCLIThemeMode 在运行时切换主题的明暗模式，用于 TUI 内部的 /theme 命令。
// 由于此时 TUI 拥有 stdin，禁用终端背景探测以避免输入冲突。
func setCLIThemeMode(mode string) cliPalette {
	// A runtime /theme switch runs inside the TUI, which owns stdin, so resolving
	// "auto" must not live-probe the terminal here.
	withoutTerminalProbe(func() {
		activeCLITheme = resolveCLIThemeWithStyle(mode, activeCLITheme.style)
	})
	refreshCLIStyles()
	return activeCLITheme
}

// setCLIThemeStyle 在运行时切换主题的样式变体，返回新主题和是否成功。
func setCLIThemeStyle(name string) (cliPalette, bool) {
	st, ok := cliThemeStyleByName(name)
	if !ok {
		return cliPalette{}, false
	}
	activeCLITheme = resolveCLIThemeWithStyle(st.mode, st.name)
	refreshCLIStyles()
	return activeCLITheme, true
}

// terminalRGB 表示从终端查询到的背景色 RGB 值。
type terminalRGB struct {
	r int
	g int
	b int
}

// looksLight 根据感知亮度公式（ITU-R BT.709）判断背景色是否偏亮。
// 亮度阈值为 150（满分 255）。
func (c terminalRGB) looksLight() bool {
	luma := 0.2126*float64(c.r) + 0.7152*float64(c.g) + 0.0722*float64(c.b)
	return luma >= 150
}

// parseOSC11Response 解析 OSC 11 终端响应字符串，提取背景色 RGB 值。
// 支持 "#RRGGBB"、"rgb:RR/GG/BB" 和 "rgba:RR/GG/BB/AA" 三种格式。
func parseOSC11Response(s string) (terminalRGB, bool) {
	idx := strings.Index(s, "]11;")
	if idx < 0 {
		return terminalRGB{}, false
	}
	payload := s[idx+len("]11;"):]
	if end := strings.IndexByte(payload, '\a'); end >= 0 {
		payload = payload[:end]
	} else if end := strings.Index(payload, "\x1b\\"); end >= 0 {
		payload = payload[:end]
	}
	payload = strings.TrimSpace(payload)
	if strings.HasPrefix(payload, "#") {
		r, g, b, ok := parseHexColor(payload)
		return terminalRGB{r, g, b}, ok
	}
	for _, prefix := range []string{"rgb:", "rgba:"} {
		if strings.HasPrefix(payload, prefix) {
			return parseOSCColorTriplet(strings.TrimPrefix(payload, prefix))
		}
	}
	return terminalRGB{}, false
}

// parseOSCColorTriplet 解析 "RR/RRRR/GGGG" 格式的 OSC 颜色三元组，
// 每个分量可以是 1-4 个十六进制字符，自动归一化到 0-255 范围。
func parseOSCColorTriplet(s string) (terminalRGB, bool) {
	parts := strings.Split(s, "/")
	if len(parts) < 3 {
		return terminalRGB{}, false
	}
	r, okR := parseOSCColorComponent(parts[0])
	g, okG := parseOSCColorComponent(parts[1])
	b, okB := parseOSCColorComponent(parts[2])
	return terminalRGB{r, g, b}, okR && okG && okB
}

// parseOSCColorComponent 将单个十六进制颜色分量归一化为 0-255 的整数值。
func parseOSCColorComponent(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 4 {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		return 0, false
	}
	max := int64(1)<<(4*len(s)) - 1
	if max <= 0 {
		return 0, false
	}
	return int(v * 255 / max), true
}

// colorFGBGLooksLight 通过 COLORFGBG 环境变量判断终端背景是否偏亮。
// 该变量格式为 "fg;bg"，bg 为 7 或 15 时表示浅色背景。
// 这是 OSC 11 检测不可用时的回退方案。
func colorFGBGLooksLight() bool {
	parts := strings.Split(os.Getenv("COLORFGBG"), ";")
	if len(parts) == 0 {
		return false
	}
	bg, err := strconv.Atoi(parts[len(parts)-1])
	return err == nil && (bg == 7 || bg == 15)
}

// fgSGR 生成设置前景色的 ANSI SGR 转义序列。
// 优先使用真彩色（24-bit RGB），不支持时回退到 xterm 256 色。
func fgSGR(c cliColor) string {
	if supportsTrueColor() && c.hex != "" {
		r, g, b, ok := parseHexColor(c.hex)
		if ok {
			return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
		}
	}
	return fmt.Sprintf("\033[38;5;%dm", c.xterm)
}

// bgSGR 生成设置背景色的 ANSI SGR 转义序列。逻辑同 fgSGR。
func bgSGR(c cliColor) string {
	if supportsTrueColor() && c.hex != "" {
		r, g, b, ok := parseHexColor(c.hex)
		if ok {
			return fmt.Sprintf("\033[48;2;%d;%d;%dm", r, g, b)
		}
	}
	return fmt.Sprintf("\033[48;5;%dm", c.xterm)
}

// parseHexColor 将 "#RRGGBB" 格式的十六进制颜色字符串解析为 R、G、B 整数值。
func parseHexColor(hex string) (int, int, int, bool) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0, 0, 0, false
	}
	r, errR := strconv.ParseUint(hex[0:2], 16, 8)
	g, errG := strconv.ParseUint(hex[2:4], 16, 8)
	b, errB := strconv.ParseUint(hex[4:6], 16, 8)
	return int(r), int(g), int(b), errR == nil && errG == nil && errB == nil
}

// supportsTrueColor 检测当前终端是否支持真彩色（24-bit）输出。
// 通过 COLORTERM 环境变量和 TERM_PROGRAM 已知支持的终端来判断。
func supportsTrueColor() bool {
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	if strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit") {
		return true
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "vscode":
		return true
	default:
		return false
	}
}

// themeFg 用指定颜色为文本添加前景色 SGR 转义序列。
func themeFg(c cliColor, s string) string {
	return sgr(fgSGR(c), s)
}

// themeLipColor 将 cliColor 转换为 lipgloss 可用的 color.Color 接口，
// 优先使用真彩色 hex 值，不支持时回退到 xterm 色号。
func themeLipColor(c cliColor) color.Color {
	if supportsTrueColor() && c.hex != "" {
		return lipgloss.Color(c.hex)
	}
	return lipgloss.Color(strconv.Itoa(c.xterm))
}

// themeStyle 创建一个带有指定前景色的 lipgloss 样式。颜色禁用时返回空样式。
func themeStyle(c cliColor) lipgloss.Style {
	if !colorEnabled {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(themeLipColor(c))
}

// withThemeFG 在已有样式上叠加前景色。颜色禁用时返回原样式。
func withThemeFG(st lipgloss.Style, c cliColor) lipgloss.Style {
	if !colorEnabled {
		return st
	}
	return st.Foreground(themeLipColor(c))
}

// withThemeBorderFG 在已有样式上叠加边框前景色。颜色禁用时返回原样式。
func withThemeBorderFG(st lipgloss.Style, c cliColor) lipgloss.Style {
	if !colorEnabled {
		return st
	}
	return st.BorderForeground(themeLipColor(c))
}

func init() {
	refreshCLIStyles()
}

// refreshCLIStyles 根据当前活跃主题刷新所有全局 UI 组件样式，
// 包括输入框、审批横幅、待办面板、状态栏、选择高亮、滚动条等。
func refreshCLIStyles() {
	inputBoxStyle = withThemeBorderFG(lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, true, false), activeCLITheme.accent).
		PaddingLeft(1)
	approvalBannerStyle = withThemeFG(withThemeBorderFG(lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, true, false), activeCLITheme.warn), activeCLITheme.warn).
		Bold(true).
		PaddingLeft(1)
	todoPanelStyle = withThemeBorderFG(lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, false, false), activeCLITheme.border).
		PaddingLeft(1)
	statusBlockStyle = themeStyle(activeCLITheme.faint)
	workingStyle = themeStyle(activeCLITheme.faint)
	compSelStyle = themeStyle(activeCLITheme.accent).Bold(true)
	choicePanelStyle = withThemeBorderFG(lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, true, false), activeCLITheme.accent).
		PaddingLeft(1)
	scrollThumbStyle = themeStyle(activeCLITheme.accent)
	scrollTrackStyle = themeStyle(activeCLITheme.faint)
}

// applyTextareaTheme 将当前主题应用到文本输入框组件，设置焦点/失焦状态下的
// 各行样式（行号、占位符、光标等）。
func applyTextareaTheme(ti *textarea.Model) {
	plain := lipgloss.NewStyle()
	weak := themeStyle(activeCLITheme.faint)
	if !colorEnabled {
		weak = plain
	}

	styles := ti.Styles()
	styles.Focused = textarea.StyleState{
		Base:             plain,
		Text:             plain,
		CursorLine:       plain,
		CursorLineNumber: weak,
		EndOfBuffer:      weak,
		LineNumber:       weak,
		Placeholder:      weak,
		Prompt:           weak,
	}
	styles.Blurred = textarea.StyleState{
		Base:             plain,
		Text:             plain,
		CursorLine:       plain,
		CursorLineNumber: weak,
		EndOfBuffer:      weak,
		LineNumber:       weak,
		Placeholder:      weak,
		Prompt:           weak,
	}
	if colorEnabled {
		styles.Cursor.Color = themeLipColor(activeCLITheme.accent)
	} else {
		styles.Cursor.Color = nil
	}
	ti.SetStyles(styles)
}

// runThemeSubcommand 处理用户输入的 /theme 命令，解析参数并切换主题。
// 支持 "/theme auto"、"/theme light"、"/theme dark" 和样式名（如 "/theme aurora"）。
func (m *chatTUI) runThemeSubcommand(input string) {
	args := tokenizeArgs(input)
	if len(args) < 2 {
		m.notice(i18n.M.ThemeHeader + "\n" + describeCLIThemes() + "\n" + i18n.M.ThemeHint)
		return
	}
	name := strings.ToLower(args[1])
	var theme cliPalette
	switch name {
	case "auto", "light", "dark":
		theme = setCLIThemeMode(name)
	default:
		next, ok := setCLIThemeStyle(name)
		if !ok {
			m.notice(fmt.Sprintf(i18n.M.ThemeUnknownFmt, name) + "\n" + describeCLIThemes())
			return
		}
		theme = next
	}
	m.refreshRuntimeTheme()
	m.notice(fmt.Sprintf(i18n.M.ThemeChangedFmt, theme.name, theme.style))

	// Persist to user config so the choice survives restart.
	m.persistTheme(name)
}

// persistTheme 将当前主题选择持久化到用户配置文件，使重启后仍保持用户选择。
func (m *chatTUI) persistTheme(inputName string) {
	path := config.UserConfigPath()
	if path == "" {
		return
	}
	edit := config.LoadForEdit(path)
	switch inputName {
	case "auto", "light", "dark":
		edit.UI.Theme = inputName
		edit.UI.ThemeStyle = activeCLITheme.style
	default:
		edit.UI.Theme = activeCLITheme.name
		edit.UI.ThemeStyle = inputName
	}
	if err := edit.SaveTo(path); err != nil {
		slog.Warn("theme: failed to persist", "path", path, "err", err)
	}
}

// refreshRuntimeTheme 在运行时刷新 TUI 组件的主题样式（加载动画、输入框等）。
func (m *chatTUI) refreshRuntimeTheme() {
	m.spinner.Style = themeStyle(activeCLITheme.accent)
	applyTextareaTheme(&m.input)
}

// describeCLIThemes 生成所有可用主题样式的描述文本，用于 /theme 命令的帮助输出。
// 当前活跃的样式前会有 "›" 标记。
func describeCLIThemes() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  auto · light · dark\n", dim("modes:"))
	for _, st := range cliThemeStyles {
		marker := "  "
		if st.name == activeCLITheme.style {
			marker = accent("› ")
		}
		fmt.Fprintf(&b, "%s%-10s %s  %s\n", marker, st.name, dim(st.mode), dim(st.description))
	}
	return strings.TrimRight(b.String(), "\n")
}

// themeArgItems 为 /theme 命令提供自动补全候选列表，包括三个模式选项（auto/light/dark）
// 和所有样式变体名称。返回候选项列表、插入位置和是否需要补全。
func (m *chatTUI) themeArgItems(val string) ([]compItem, int, bool) {
	cmdEnd := strings.IndexAny(val, " \t")
	if cmdEnd < 0 || val[:cmdEnd] != "/theme" {
		return nil, 0, false
	}
	from := strings.LastIndexAny(val, " \t") + 1
	prior := strings.Fields(val[:from])
	if len(prior) != 1 {
		return nil, from, true
	}
	cur := strings.ToLower(val[from:])
	items := []struct {
		label string
		mode  string
		desc  string
	}{
		{label: "auto", mode: "mode", desc: "detect terminal background"},
		{label: "light", mode: "mode", desc: "force light shell"},
		{label: "dark", mode: "mode", desc: "force dark shell"},
	}
	var out []compItem
	for _, it := range items {
		if cur != "" && !strings.HasPrefix(it.label, cur) {
			continue
		}
		out = append(out, compItem{label: it.label, insert: it.label, hint: it.mode + " · " + it.desc})
	}
	for _, st := range cliThemeStyles {
		if cur != "" && !strings.HasPrefix(st.name, cur) {
			continue
		}
		hint := st.mode + " · " + st.description
		if st.name == activeCLITheme.style {
			hint = i18n.M.ArgThemeCurrent
		}
		out = append(out, compItem{label: st.name, insert: st.name, hint: hint})
	}
	return out, from, true
}
