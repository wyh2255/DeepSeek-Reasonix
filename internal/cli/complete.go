// complete.go 实现了 CLI 输入框的自动补全菜单系统。
// 支持三种补全模式：
//   - 斜杠命令补全（compSlash）: 输入 "/" 时触发，补全命令名称
//   - 斜杠命令参数补全（compSlashArg）: 命令后的结构化参数补全
//   - @ 引用补全（compAt）: 输入 "@" 时触发，补全文件路径和 MCP 资源
//
// 补全菜单使用模糊匹配算法，支持子序列匹配和前缀优先排序。

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"reasonix/internal/control"
	"reasonix/internal/fileref"
	"reasonix/internal/i18n"
	"reasonix/internal/skill"
)

// compKind distinguishes the two completion menus.
type compKind int

const (
	compSlash    compKind = iota // slash command names, while the line is a bare "/word"
	compSlashArg                 // a structured argument of a slash command (e.g. "/mcp remove <name>")
	compAt                       // @-references (files / MCP resources)
)

// compItem is one menu row: label shown, insert applied on accept, hint dimmed.
// descend marks a directory entry — accepting it fills the input and re-opens
// the menu one level deeper instead of closing.
// compItem 表示补全菜单中的一行条目。
// label 为显示文本，insert 为选中后插入的内容，hint 为暗色提示信息。
// descend 标记该条目是否为目录（选中后会展开下一级而非关闭菜单）。
type compItem struct {
	label   string
	insert  string
	hint    string
	descend bool
}

// completion is the live autocomplete menu state. Empty value = inactive.
// replaceFrom is the byte offset in the input where the completed token starts
// (0 for a slash line, the '@' index for an @-reference).
// completion 表示当前活跃的补全菜单状态。
// 空值表示菜单未激活。replaceFrom 记录补全替换的起始字节偏移量。
type completion struct {
	active      bool
	kind        compKind
	items       []compItem
	sel         int
	replaceFrom int
}

const (
	// maxCompRows caps how many menu rows show at once; the list windows around
	// the selection when longer.
	maxCompRows = 8
	// maxCompItems caps how many entries a single directory contributes, so a
	// pathologically large directory can't blow up the menu — we read only one
	// level (os.ReadDir), never the whole tree.
	maxCompItems = 200
	// maxFileSearchItems caps basename search results for bare @tokens.
	maxFileSearchItems = 20
)

// slashItems is the full set of slash commands offered for completion: the
// built-in verbs, custom commands, skills (each as "/<name>"), and MCP prompts.
func (m *chatTUI) slashItems() []compItem {
	items := []compItem{
		{label: "/compact", insert: "/compact ", hint: i18n.M.CmdCompact},
		{label: "/new", insert: "/new ", hint: i18n.M.CmdNew},
		{label: "/clear", insert: "/clear", hint: i18n.M.CmdClear},
		{label: "/resume", insert: "/resume ", hint: i18n.M.CmdResume},
		{label: "/rename", insert: "/rename ", hint: i18n.M.CmdRename},
		{label: "/rewind", insert: "/rewind", hint: i18n.M.CmdRewind},
		{label: "/tree", insert: "/tree", hint: i18n.M.CmdTree},
		{label: "/branch", insert: "/branch ", hint: i18n.M.CmdBranch},
		{label: "/switch", insert: "/switch ", hint: i18n.M.CmdSwitchBranch},
		{label: "/mcp", insert: "/mcp", hint: i18n.M.CmdMcp},
		{label: "/model", insert: "/model ", hint: i18n.M.CmdModel, descend: true},
		{label: "/provider", insert: "/provider ", hint: i18n.M.CmdProvider, descend: true},
		{label: "/skills", insert: "/skills", hint: i18n.M.CmdSkill},
		{label: "/reload-cmd", insert: "/reload-cmd", hint: i18n.M.CmdReloadCmd},
		{label: "/hooks", insert: "/hooks ", hint: i18n.M.CmdHooks, descend: true},
		{label: "/paste-image", insert: "/paste-image", hint: i18n.M.CmdPasteImage},
		{label: "/output-style", insert: "/output-style", hint: i18n.M.CmdOutputStyle},
		{label: "/verbose", insert: "/verbose", hint: i18n.M.CmdVerbose},
		{label: "/diff-fold", insert: "/diff-fold", hint: i18n.M.CmdDiffFold},
		{label: "/sandbox", insert: "/sandbox", hint: i18n.M.CmdSandbox},
		{label: "/effort", insert: "/effort ", hint: i18n.M.CmdEffort, descend: true},
		{label: "/auto-plan", insert: "/auto-plan ", hint: i18n.M.CmdAutoPlan, descend: true},
		{label: "/reasoning-language", insert: "/reasoning-language ", hint: i18n.M.CmdReasonLang, descend: true},
		{label: "/theme", insert: "/theme ", hint: i18n.M.CmdTheme, descend: true},
		{label: "/language", insert: "/language ", hint: i18n.M.CmdLanguage, descend: true},
		{label: "/help", insert: "/help ", hint: i18n.M.CmdHelp},
		{label: "/memory", insert: "/memory ", hint: i18n.M.CmdMemory},
		{label: "/migrate", insert: "/migrate", hint: i18n.M.CmdMigrate},
		{label: "/goal", insert: "/goal ", hint: i18n.M.CmdGoal, descend: true},
		{label: "/remember", insert: "/remember ", hint: i18n.M.CmdRemember},
		{label: "/forget", insert: "/forget ", hint: i18n.M.CmdForget},
		{label: "/quit", insert: "/quit", hint: i18n.M.CmdQuit},
	}
	for _, c := range m.commands {
		items = append(items, compItem{label: "/" + c.Name, insert: "/" + c.Name + " ", hint: c.Description})
	}
	for _, s := range m.skills {
		hint := s.Description
		if s.RunAs == skill.RunSubagent {
			hint = "🧬 " + hint
		}
		items = append(items, compItem{label: "/" + s.Name, insert: "/" + s.Name + " ", hint: hint})
	}
	for _, p := range m.prompts() {
		items = append(items, compItem{label: "/" + p.Name, insert: "/" + p.Name + " ", hint: p.Description})
	}
	return items
}

// updateCompletion recomputes the menu from the current input: a slash menu
// while the line is a single "/word" token, or an @-reference menu while the
// token under the cursor is "@…".
// updateCompletion 根据当前输入内容重新计算补全菜单。
// 优先检查 @ 引用 token，然后检查斜杠命令及其参数。
// 当输入不匹配任何补全模式时关闭菜单。
func (m *chatTUI) updateCompletion() {
	val := m.input.Value()

	// An @-reference token under the cursor wins — it can appear mid-line, even
	// inside a slash command's arguments (e.g. "/review @file").
	if at, token, ok := activeAtToken(val); ok {
		if items := m.atItems(token); len(items) > 0 {
			m.setCompletion(compAt, items, at)
			return
		}
	}

	if strings.HasPrefix(val, "/") {
		if items, from, ok := m.explicitSubcommandItems(val); ok && len(items) > 0 {
			m.setCompletion(compSlashArg, items, from)
			return
		}
		if !strings.ContainsAny(val, " \t\n") {
			// Still naming the command itself.
			if items := fuzzyFilterSlash(m.slashItems(), val); len(items) > 0 {
				m.setCompletion(compSlash, items, 0)
				return
			}
		} else if m.bareSubcommandSpace(val) {
			m.completion = completion{}
			return
		} else if items, from, ok := m.slashArgItems(val); ok && len(items) > 0 {
			// Past the command word — complete its structured arguments.
			m.setCompletion(compSlashArg, items, from)
			return
		}
	}

	m.completion = completion{}
}

// slashArgItems completes the arguments of a slash command (everything after the
// command word). It returns the menu items, the byte offset where the current
// token begins (replaceFrom, so accept replaces just that token), and whether
// anything applied. Only commands with structured arguments participate —
// currently /mcp; custom commands and MCP prompts take free-form template args,
// so they yield nothing.
// slashArgItems 为斜杠命令的参数提供补全项。
// 委托给各个具体的参数补全函数（分支、resume、主题等），
// 最后通过共享的 control.SlashArgItems 逻辑处理通用参数。
func (m *chatTUI) slashArgItems(val string) ([]compItem, int, bool) {
	if items, from, ok := m.branchArgItems(val); ok {
		return items, from, len(items) > 0
	}
	if items, from, ok := m.resumeArgItems(val); ok {
		return items, from, len(items) > 0
	}
	if items, from, ok := m.themeArgItems(val); ok {
		return items, from, len(items) > 0
	}
	// Delegate to the shared completion logic so the chat TUI and the desktop
	// offer identical sub-command hints. We supply the data from the TUI's own
	// cached lists (no live controller needed), build the items, and adapt them
	// to compItem.
	items, from := control.SlashArgItems(val, m.slashArgData())
	if len(items) == 0 {
		return nil, 0, false
	}
	return slashItemsToComps(items), from, true
}

// slashArgData 构建斜杠命令参数补全所需的数据集合。
// 包含当前技能列表、模型引用、提供商名称、MCP 服务器名等信息。
func (m *chatTUI) slashArgData() control.ArgData {
	curProvider := ""
	if parts := strings.SplitN(m.modelRef, "/", 2); len(parts) == 2 {
		curProvider = parts[0]
	}
	data := control.ArgData{
		Skills:          m.skills,
		ModelRefs:       modelRefs(),
		CurrentModel:    m.modelRef,
		ProviderNames:   providerNames(),
		CurrentProvider: curProvider,
	}
	if m.ctrl != nil {
		data.DisabledSkills = m.ctrl.DisabledSkills()
		data.ConfiguredMCP = m.ctrl.ConfiguredMCPNames()
		data.DisconnectedMCP = m.ctrl.DisconnectedMCPNames()
	}
	if m.host != nil {
		data.ServerNames = m.host.ServerNames()
	}
	return data
}

// explicitSubcommandItems 处理以 "?" 结尾的显式子命令补全请求。
// 例如输入 "/mcp?" 会显示 mcp 的所有子命令选项。
func (m *chatTUI) explicitSubcommandItems(val string) ([]compItem, int, bool) {
	cmd, ok := strings.CutSuffix(val, "?")
	if !ok {
		return nil, 0, false
	}
	switch cmd {
	case "/mcp", "/skill", "/skills":
	default:
		return nil, 0, false
	}
	items, _ := control.SlashArgItems(cmd+" ", m.slashArgData())
	if len(items) == 0 {
		return nil, 0, false
	}
	out := slashItemsToComps(items)
	for i := range out {
		out[i].insert = " " + out[i].insert
	}
	return out, len(cmd), true
}

// bareSubcommandSpace 判断输入是否为某个子命令后仅跟空白字符的状态。
// 用于在特定命令（如 /mcp、/skills）后关闭补全菜单，避免干扰。
func (m *chatTUI) bareSubcommandSpace(val string) bool {
	if !strings.ContainsAny(val, " \t") || strings.TrimRight(val, " \t") == val {
		return false
	}
	fields := strings.Fields(val)
	if len(fields) != 1 {
		return false
	}
	switch fields[0] {
	case "/mcp", "/skill", "/skills":
		return true
	default:
		return false
	}
}

// slashItemsToComps 将通用的 SlashItem 切片转换为 CLI 专用的 compItem 切片。
func slashItemsToComps(items []control.SlashItem) []compItem {
	out := make([]compItem, len(items))
	for i, it := range items {
		out[i] = compItem{label: it.Label, insert: it.Insert, hint: it.Hint, descend: it.Descend}
	}
	return out
}

// branchArgItems 为 /switch 命令提供分支名称/ID 的补全项。
// 根据用户输入的前缀过滤匹配的分支，显示分支的回合数和预览信息。
func (m *chatTUI) branchArgItems(val string) ([]compItem, int, bool) {
	cmdEnd := strings.IndexAny(val, " \t")
	if cmdEnd < 0 || val[:cmdEnd] != "/switch" {
		return nil, 0, false
	}
	from := strings.LastIndexAny(val, " \t") + 1
	prior := strings.Fields(val[:from])
	if len(prior) != 1 || m.ctrl == nil {
		return nil, from, true
	}
	branches, err := m.ctrl.Branches()
	if err != nil {
		return nil, from, true
	}
	cur := strings.ToLower(val[from:])
	var out []compItem
	for _, b := range branches {
		label := b.ID
		if cur != "" && !strings.HasPrefix(strings.ToLower(label), cur) &&
			!strings.HasPrefix(strings.ToLower(b.Name), cur) {
			continue
		}
		hint := b.Name
		if hint == "" {
			hint = b.Preview
		}
		if hint != "" {
			hint = fmt.Sprintf("%d turns · %s", b.Turns, hint)
		}
		out = append(out, compItem{label: label, insert: label, hint: hint})
	}
	return out, from, true
}

// setCompletion installs items, preserving the selection index only while the
// same menu kind stays open.
func (m *chatTUI) setCompletion(kind compKind, items []compItem, replaceFrom int) {
	sel := 0
	if m.completion.active && m.completion.kind == kind && m.completion.sel < len(items) {
		sel = m.completion.sel
	}
	m.completion = completion{active: true, kind: kind, items: items, sel: sel, replaceFrom: replaceFrom}
}

// fuzzyFilterSlash returns the slash-menu items that match query as a
// case-insensitive subsequence of their label, with prefix hits ranked first
// (each group preserved in the input order from slashItems). An empty query
// matches everything — the same behavior the old prefix filter had, since
// every label trivially starts with "". A query that matches nothing returns
// nil so the caller can fall through and close the menu.
func fuzzyFilterSlash(items []compItem, query string) []compItem {
	if query == "" {
		out := make([]compItem, len(items))
		copy(out, items)
		return out
	}
	lq := strings.ToLower(query)
	var prefix, rest []compItem
	for _, it := range items {
		l := strings.ToLower(it.label)
		switch {
		case strings.HasPrefix(l, lq):
			prefix = append(prefix, it)
		case subsequenceMatch(l, lq):
			rest = append(rest, it)
		}
	}
	if len(prefix) == 0 && len(rest) == 0 {
		return nil
	}
	out := make([]compItem, 0, len(prefix)+len(rest))
	out = append(out, prefix...)
	out = append(out, rest...)
	return out
}

// subsequenceMatch reports whether query appears in target as a case-folded
// subsequence (each rune of query in order, not necessarily contiguous). It is
// the matcher behind the slash-menu fuzzy filter: typing "/modl" matches
// "/model", "/memory", or any other label where m-o-d-l appear in that order.
// Callers must pass already case-folded strings; an empty query matches
// every target, so callers that want a "no match" signal on the empty input
// should check that first.
func subsequenceMatch(target, query string) bool {
	if query == "" {
		return true
	}
	qr := []rune(query)
	ti := 0
	for _, r := range target {
		if r == qr[ti] {
			ti++
			if ti == len(qr) {
				return true
			}
		}
	}
	return false
}

// activeAtToken finds the @-reference token ending at the cursor (assumed at the
// input's end). The '@' must start the line or follow whitespace, so emails
// like "a@b" don't trigger it. Returns the '@' offset and the text after it.
func activeAtToken(val string) (int, string, bool) {
	for i := len(val) - 1; i >= 0; i-- {
		switch val[i] {
		case ' ', '\t', '\n':
			return 0, "", false // hit whitespace before an '@' → no active token
		case '@':
			if i == 0 || val[i-1] == ' ' || val[i-1] == '\t' || val[i-1] == '\n' {
				return i, val[i+1:], true
			}
			return 0, "", false
		}
	}
	return 0, "", false
}

// atItems builds the @-reference menu for a token. A "server:uri" token whose
// server is connected lists that server's MCP resources; otherwise the token is
// a path and we list one directory level (never a recursive walk), plus — at the
// top level — any matching MCP resources.
func (m *chatTUI) atItems(token string) []compItem {
	if i := strings.Index(token, ":"); i > 0 && m.isMCPServer(token[:i]) {
		return m.resourceItems(token[:i], token[i+1:])
	}
	return m.fileItems(token)
}

// fileItems lists one directory level for a path token. dir is the part up to
// the last '/', frag the part after; entries of dir starting with frag are
// offered (directories descend, files complete). Hidden entries are skipped
// unless frag starts with '.'. Top-level tokens also surface MCP resources.
func (m *chatTUI) fileItems(token string) []compItem {
	dir, frag := splitPathToken(token)
	workspaceRoot := ""
	if m.ctrl != nil {
		workspaceRoot = m.ctrl.WorkspaceRoot()
	}
	readDir := dir
	if workspaceRoot != "" {
		if readDir == "" {
			readDir = workspaceRoot
		} else if !filepath.IsAbs(readDir) {
			readDir = filepath.Join(workspaceRoot, filepath.FromSlash(readDir))
		}
	} else if readDir == "" {
		readDir = "."
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		entries = nil
	}
	// Directories first, then files; ReadDir is already name-sorted.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].IsDir() && !entries[j].IsDir()
	})

	showHidden := strings.HasPrefix(frag, ".")
	var items []compItem
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, frag) {
			continue
		}
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			items = append(items, compItem{label: name + "/", insert: "@" + dir + name + "/", hint: "dir", descend: true})
		} else {
			items = append(items, compItem{label: name, insert: "@" + dir + name})
		}
		if len(items) >= maxCompItems {
			break
		}
	}

	// At the top level (still naming the first segment) MCP resources share the
	// '@' namespace, so offer the matching ones too.
	if !strings.Contains(token, "/") {
		seen := map[string]bool{}
		for _, it := range items {
			seen[strings.TrimPrefix(it.insert, "@")] = true
		}
		remaining := maxCompItems - len(items)
		if remaining > maxFileSearchItems {
			remaining = maxFileSearchItems
		}
		results := m.searchFileRefs(frag)
		if len(results) > remaining {
			results = results[:remaining]
		}
		for _, path := range results {
			if seen[path] {
				continue
			}
			items = append(items, compItem{label: path, insert: "@" + path, hint: "file"})
			if len(items) >= maxCompItems {
				break
			}
		}
		items = append(items, m.resourceItems("", token)...)
	}
	return items
}

// searchFileRefs memoizes the bounded basename walk so re-rendering the menu
// for an unchanged @token fragment doesn't re-walk the workspace each keystroke.
func (m *chatTUI) searchFileRefs(frag string) []string {
	if m.fileSearchCache == nil {
		m.fileSearchCache = map[string][]string{}
	}
	if r, ok := m.fileSearchCache[frag]; ok {
		return r
	}
	searchRoot := "."
	if m.ctrl != nil {
		if wr := m.ctrl.WorkspaceRoot(); wr != "" {
			searchRoot = wr
		}
	}
	results := fileref.Search(searchRoot, frag, maxFileSearchItems)
	paths := make([]string, 0, len(results))
	for _, r := range results {
		paths = append(paths, r.Path)
	}
	m.fileSearchCache[frag] = paths
	return paths
}

// splitPathToken splits a path token into (dir, frag): dir keeps its trailing
// slash ("internal/" ), frag is the segment being typed.
func splitPathToken(token string) (dir, frag string) {
	if i := strings.LastIndex(token, "/"); i >= 0 {
		return token[:i+1], token[i+1:]
	}
	return "", token
}

// isMCPServer reports whether name is a connected MCP server.
func (m *chatTUI) isMCPServer(name string) bool {
	if m.host == nil {
		return false
	}
	for _, s := range m.host.ServerNames() {
		if s == name {
			return true
		}
	}
	return false
}

// resourceItems lists MCP resources as @server:uri completions. When server is
// "" (top level) it matches by the whole "server:uri" prefix; otherwise it lists
// the named server's resources filtered by the uri prefix.
func (m *chatTUI) resourceItems(server, frag string) []compItem {
	if m.host == nil {
		return nil
	}
	var items []compItem
	for _, r := range m.host.Resources() {
		ref := r.Server + ":" + r.URI
		switch {
		case server == "":
			if !strings.HasPrefix(ref, frag) {
				continue
			}
		case r.Server == server:
			if !strings.HasPrefix(r.URI, frag) {
				continue
			}
		default:
			continue
		}
		label := r.Name
		if label == "" {
			label = "resource"
		}
		items = append(items, compItem{label: "@" + ref, insert: "@" + ref, hint: label})
	}
	return items
}

// moveCompletion advances the selection by delta, wrapping around.
// moveCompletion 将补全菜单的选择光标移动指定的偏移量，支持循环滚动。
func (m *chatTUI) moveCompletion(delta int) {
	n := len(m.completion.items)
	if n == 0 {
		return
	}
	m.completion.sel = ((m.completion.sel+delta)%n + n) % n
}

// completionExactLabel 判断当前输入值是否与选中的补全项标签完全匹配。
// 用于判断是否应关闭补全菜单（避免重复选中同一项）。
func (m *chatTUI) completionExactLabel() bool {
	if !m.completion.active || m.completion.sel >= len(m.completion.items) {
		return false
	}
	val := strings.TrimSpace(m.input.Value())
	return val == m.completion.items[m.completion.sel].label
}

// completionBareOverlayCommand 判断当前输入是否为需要覆盖显示的裸命令。
// 当输入仅为 "/mcp" 或 "/skills" 时返回 true。
func (m *chatTUI) completionBareOverlayCommand() bool {
	switch strings.TrimSpace(m.input.Value()) {
	case "/mcp", "/skills":
		return true
	default:
		return false
	}
}

// completionSelectedInsertPresent 判断选中补全项的插入文本是否已存在于输入中。
// 用于避免重复插入相同内容。
func (m *chatTUI) completionSelectedInsertPresent() bool {
	if !m.completion.active || m.completion.sel >= len(m.completion.items) {
		return false
	}
	val := m.input.Value()
	if m.completion.replaceFrom > len(val) {
		return false
	}
	return val[m.completion.replaceFrom:] == m.completion.items[m.completion.sel].insert
}

// acceptCompletion applies the selected item to the input, then recomputes the
// menu from the new value: it re-opens one level deeper (a descended directory
// or a freshly completed command's arguments) or closes when nothing applies.
func (m *chatTUI) acceptCompletion() {
	if m.completion.sel >= len(m.completion.items) {
		m.completion = completion{}
		return
	}
	it := m.completion.items[m.completion.sel]
	val := m.input.Value()
	rf := m.completion.replaceFrom
	if rf > len(val) {
		rf = len(val)
	}
	m.input.SetValue(val[:rf] + it.insert)
	m.input.CursorEnd()
	if it.descend || strings.HasSuffix(it.insert, " ") {
		// 目录项或以空格结尾的项：重新计算补全以展开下一级
		m.updateCompletion()
		return
	}
	// 重新过滤以支持参数补全（如 /resume 后的编号会话列表）
	m.updateCompletion()
	// 如果补全重新打开且仅剩用户刚选中的同一项（即 token 已完整输入），
	// 则关闭菜单，使下一次 Enter 能提交命令而非再次被补全拦截。
	if m.completion.active && len(m.completion.items) == 1 {
		tok := m.input.Value()[m.completion.replaceFrom:]
		if tok == m.completion.items[0].insert {
			m.completion = completion{}
		}
	}
}

// compSelStyle 是补全菜单选中项的样式（反色显示）。
var compSelStyle lipgloss.Style

const completionPadCell = "\u00a0"

// padCompletionLine pads completion rows with NBSPs instead of ASCII spaces.
// Ultraviolet treats trailing ASCII spaces as clearable cells and may emit EL
// or ECH erase sequences; mintty can leave stale CJK glyph cells after those
// erases. NBSP is visually blank but forces the renderer to overwrite cells.
func padCompletionLine(s string, w int) string {
	pad := w - visibleWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(completionPadCell, pad)
}

// renderCompletion draws the menu above the input box: matching items, windowed
// around the selection, the current row highlighted, hints dimmed. Every line is
// padded to m.width with non-clearable blank cells so bubbletea's delta renderer
// has no ordinary trailing-space run to collapse into EL/ECH erase sequences.
// That avoids ghost cells on terminals (mintty) with unreliable erases after
// wide CJK glyphs.
// renderCompletion 渲染补全菜单，显示在输入框上方。
// 使用窗口化显示（最多 maxCompRows 行），当前选中项高亮。
// 每行使用 NBSP 填充以避免某些终端的擦除问题。
func (m chatTUI) renderCompletion() string {
	if !m.completion.active || len(m.completion.items) == 0 {
		return ""
	}
	items := m.completion.items
	start := 0
	if len(items) > maxCompRows {
		start = m.completion.sel - maxCompRows/2
		if start < 0 {
			start = 0
		}
		if start > len(items)-maxCompRows {
			start = len(items) - maxCompRows
		}
	}
	end := start + maxCompRows
	if end > len(items) {
		end = len(items)
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		it := items[i]
		var line string
		if i == m.completion.sel {
			line = accent("› ") + compSelStyle.Render(it.label)
		} else {
			line = "  " + it.label
		}
		if it.hint != "" {
			line += "  " + dim(it.hint)
		}
		b.WriteString(padCompletionLine(line, m.width))
		b.WriteByte('\n')
	}
	// A key-hint footer so users discover Tab — many won't know it accepts a
	// completion, let alone descends into a folder.
	hint := i18n.M.CompHintSlash
	if m.completion.kind == compAt {
		hint = i18n.M.CompHintFile
	}
	b.WriteString(padCompletionLine(dim(hint), m.width))
	return b.String()
}
