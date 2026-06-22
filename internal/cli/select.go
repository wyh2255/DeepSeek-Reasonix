// select.go 实现了终端中的交互式单选和多选菜单组件。
// 支持键盘导航（↑/↓/j/k）、搜索过滤（/）、滚动视口，
// 适用于 TTY 环境下的 setup 流程和 CLI 工具选择。
package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"reasonix/internal/i18n"
)

// errCancelled 当用户中止选择（按 q 或 Ctrl-C）时由 selectOne/selectMany 返回。
var errCancelled = errors.New("selection cancelled")

// menuItem 表示菜单中的一个选项，包含名称和描述。
type menuItem struct {
	name string
	desc string
}

// termHeight 返回终端的行数，出错时回退到 24。
func termHeight(fd int) int {
	_, h, err := term.GetSize(fd)
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// fixedLines 返回每帧渲染的非菜单项行数：标题标签、空白分隔符、
// 向上滚动指示器、向下滚动指示器。搜索模式下额外增加一行搜索栏。
func fixedLines(searching bool) int {
	n := 4 // header + blank + scroll-up + scroll-down
	if searching {
		n++ // search bar
	}
	return n
}

// maxViewport 计算从可用终端行数中减去固定行数后能容纳的菜单项行数，
// 至少保留 1 行。
func maxViewport(totalItems, termRows int, searching bool) int {
	avail := termRows - fixedLines(searching)
	if avail < 1 {
		avail = 1
	}
	if totalItems < avail {
		return totalItems
	}
	return avail
}

// renderSearchBar 在搜索模式激活时绘制搜索输入行。
func renderSearchBar(w *os.File, query string) {
	fmt.Fprintf(w, "\r\033[K%s %s\n", accent("🔍"), query+"_")
}

// filterMenuItems 返回名称或描述中包含查询字符串（不区分大小写）的菜单项。
func filterMenuItems(items []menuItem, query string) []menuItem {
	if query == "" {
		return items
	}
	lq := strings.ToLower(query)
	var out []menuItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.name), lq) || strings.Contains(strings.ToLower(it.desc), lq) {
			out = append(out, it)
		}
	}
	return out
}

// selectOne 渲染一个交互式单选菜单，使用方向键（或 j/k）导航，
// 按 Enter 确认，按 q 或 Ctrl-C 中止。它将终端置于 raw 模式，
// 因此需要 TTY 环境。当菜单项超过终端高度时，只显示视口大小的窗口，
// 并带有滚动指示器。按 '/' 进入搜索模式按关键词过滤选项。
func selectOne(label string, items []menuItem) (int, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 0, err
	}
	defer term.Restore(fd, old)

	w := os.Stdout
	th := termHeight(fd)

	// search state
	searching := false
	searchQuery := ""
	filtered := items
	filterIdx := make([]int, len(items))
	for i := range items {
		filterIdx[i] = i
	}

	sel := 0
	scroll := 0
	prevLines := 0 // lines printed in the previous frame; 0 = first frame

	render := func() {
		n := len(filtered)
		vp := maxViewport(n, th, searching)
		// adjust scroll to keep sel visible
		if sel < scroll {
			scroll = sel
		}
		if sel >= scroll+vp {
			scroll = sel - vp + 1
		}
		if scroll < 0 {
			scroll = 0
		}

		// scroll-up indicator (always 1 line)
		if n > 0 && scroll > 0 {
			fmt.Fprintf(w, "\r\033[K%s\n", dim(fmt.Sprintf(i18n.M.SelectMoreAboveFmt, scroll)))
		} else {
			fmt.Fprintf(w, "\r\033[K\r\n")
		}

		// menu rows
		end := scroll + vp
		if end > n {
			end = n
		}
		for i := scroll; i < end; i++ {
			it := filtered[i]
			name := fmt.Sprintf("%-10s", it.name)
			if i == sel {
				fmt.Fprintf(w, "\r\033[K%s\r\n", reverse(fmt.Sprintf(" ❯ %s %s ", name, it.desc)))
			} else {
				fmt.Fprintf(w, "\r\033[K   %s %s\r\n", name, dim(it.desc))
			}
		}
		// if fewer items than viewport, pad with blank lines so the frame
		// height stays constant
		for i := end - scroll; i < vp; i++ {
			fmt.Fprintf(w, "\r\033[K\r\n")
		}

		// scroll-down indicator (always 1 line)
		if n > 0 && end < n {
			fmt.Fprintf(w, "\r\033[K%s\n", dim(fmt.Sprintf(i18n.M.SelectMoreBelowFmt, n-end)))
		} else {
			fmt.Fprintf(w, "\r\033[K\r\n")
		}
	}

	drawHeader := func() {
		if searching {
			fmt.Fprintf(w, "\r\033[K%s %s  %s\r\n\r\n", accent("▌"), bold(label), dim(i18n.M.SelectSearchHint))
			renderSearchBar(w, searchQuery)
		} else {
			fmt.Fprintf(w, "\r\033[K%s %s  %s\r\n\r\n", accent("▌"), bold(label), dim(i18n.M.SelectOneHint))
		}
	}

	redraw := func() {
		if prevLines > 0 {
			fmt.Fprintf(w, "\033[%dA", prevLines)
		}
		drawHeader()
		render()
		// Clear everything below the current frame so stale rows from a taller
		// previous frame don't linger.
		fmt.Fprint(w, "\033[J")
		prevLines = fixedLines(searching) + maxViewport(len(filtered), th, searching)
	}

	redraw() // initial draw

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 0, err
		}
		k := buf[:n]

		if searching {
			switch {
			case k[0] == 27: // Esc — exit search
				searching = false
				searchQuery = ""
				filtered = items
				filterIdx = make([]int, len(items))
				for i := range items {
					filterIdx[i] = i
				}
				sel = 0
				scroll = 0
				redraw()
			case k[0] == '\r' || k[0] == '\n':
				if len(filtered) > 0 {
					fmt.Fprint(w, "\r\n")
					return filterIdx[sel], nil
				}
			case k[0] == 127 || k[0] == 8: // backspace
				if len(searchQuery) > 0 {
					searchQuery = searchQuery[:len(searchQuery)-1]
					filtered = filterMenuItems(items, searchQuery)
					filterIdx = filterIndices(items, searchQuery)
					sel = 0
					scroll = 0
					redraw()
				}
			case k[0] == 3: // Ctrl-C
				fmt.Fprint(w, "\r\n")
				return 0, errCancelled
			case k[0] >= 32 && k[0] < 127: // printable
				searchQuery += string(k[0])
				filtered = filterMenuItems(items, searchQuery)
				filterIdx = filterIndices(items, searchQuery)
				sel = 0
				scroll = 0
				redraw()
			default:
				continue
			}
			continue
		}

		switch {
		case k[0] == '\r' || k[0] == '\n':
			fmt.Fprint(w, "\r\n")
			return filterIdx[sel], nil
		case k[0] == 3 || k[0] == 'q': // Ctrl-C or q
			fmt.Fprint(w, "\r\n")
			return 0, errCancelled
		case k[0] == '/': // enter search mode
			searching = true
			searchQuery = ""
			redraw()
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A': // up
			if sel > 0 {
				sel--
			}
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B': // down
			if sel < len(filtered)-1 {
				sel++
			}
		case k[0] == 'k':
			if sel > 0 {
				sel--
			}
		case k[0] == 'j':
			if sel < len(filtered)-1 {
				sel++
			}
		default:
			continue // ignore other keys, no redraw
		}
		redraw()
	}
}

// selectMany 渲染一个交互式多选菜单：方向键（或 j/k）移动，空格切换选中状态，
// 按 Enter 确认（至少需要选择一项），按 q 或 Ctrl-C 中止。
// 返回选中的索引列表（按顺序），需要 TTY 环境。
// 当菜单项超过终端高度时只显示视口大小的窗口。按 '/' 进入搜索模式。
func selectMany(label string, items []menuItem) ([]int, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	defer term.Restore(fd, old)

	w := os.Stdout
	th := termHeight(fd)

	// search state
	searching := false
	searchQuery := ""
	filtered := items
	filterIdx := make([]int, len(items))
	for i := range items {
		filterIdx[i] = i
	}

	cur := 0
	checked := make([]bool, len(items))
	scroll := 0
	prevLines := 0

	render := func() {
		n := len(filtered)
		vp := maxViewport(n, th, searching)
		if cur < scroll {
			scroll = cur
		}
		if cur >= scroll+vp {
			scroll = cur - vp + 1
		}
		if scroll < 0 {
			scroll = 0
		}

		if n > 0 && scroll > 0 {
			fmt.Fprintf(w, "\r\033[K%s\n", dim(fmt.Sprintf(i18n.M.SelectMoreAboveFmt, scroll)))
		} else {
			fmt.Fprintf(w, "\r\033[K\r\n")
		}

		end := scroll + vp
		if end > n {
			end = n
		}
		for i := scroll; i < end; i++ {
			it := filtered[i]
			origIdx := filterIdx[i]
			box := "[ ]"
			if checked[origIdx] {
				box = "[x]"
			}
			name := fmt.Sprintf("%-14s", it.name)
			if i == cur {
				fmt.Fprintf(w, "\r\033[K%s\r\n", reverse(fmt.Sprintf(" ❯ %s %s %s ", box, name, it.desc)))
			} else {
				fmt.Fprintf(w, "\r\033[K   %s %s %s\r\n", box, name, dim(it.desc))
			}
		}
		for i := end - scroll; i < vp; i++ {
			fmt.Fprintf(w, "\r\033[K\r\n")
		}

		if n > 0 && end < n {
			fmt.Fprintf(w, "\r\033[K%s\n", dim(fmt.Sprintf(i18n.M.SelectMoreBelowFmt, n-end)))
		} else {
			fmt.Fprintf(w, "\r\033[K\r\n")
		}
	}

	drawHeader := func() {
		if searching {
			fmt.Fprintf(w, "\r\033[K%s %s  %s\r\n\r\n", accent("▌"), bold(label), dim(i18n.M.SelectSearchHint))
			renderSearchBar(w, searchQuery)
		} else {
			fmt.Fprintf(w, "\r\033[K%s %s  %s\r\n\r\n", accent("▌"), bold(label), dim(i18n.M.SelectManyHint))
		}
	}

	redraw := func() {
		if prevLines > 0 {
			fmt.Fprintf(w, "\033[%dA", prevLines)
		}
		drawHeader()
		render()
		fmt.Fprint(w, "\033[J")
		prevLines = fixedLines(searching) + maxViewport(len(filtered), th, searching)
	}

	redraw()

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil, err
		}
		k := buf[:n]

		if searching {
			switch {
			case k[0] == 27: // Esc — exit search
				searching = false
				searchQuery = ""
				filtered = items
				filterIdx = make([]int, len(items))
				for i := range items {
					filterIdx[i] = i
				}
				cur = 0
				scroll = 0
				redraw()
			case k[0] == '\r' || k[0] == '\n':
				var out []int
				for i, c := range checked {
					if c {
						out = append(out, i)
					}
				}
				if len(out) == 0 {
					continue
				}
				fmt.Fprint(w, "\r\n")
				return out, nil
			case k[0] == ' ':
				if len(filtered) > 0 {
					origIdx := filterIdx[cur]
					checked[origIdx] = !checked[origIdx]
				}
			case k[0] == 127 || k[0] == 8: // backspace
				if len(searchQuery) > 0 {
					searchQuery = searchQuery[:len(searchQuery)-1]
					filtered = filterMenuItems(items, searchQuery)
					filterIdx = filterIndices(items, searchQuery)
					cur = 0
					scroll = 0
					redraw()
				}
			case k[0] == 3: // Ctrl-C
				fmt.Fprint(w, "\r\n")
				return nil, errCancelled
			case k[0] >= 32 && k[0] < 127:
				searchQuery += string(k[0])
				filtered = filterMenuItems(items, searchQuery)
				filterIdx = filterIndices(items, searchQuery)
				cur = 0
				scroll = 0
				redraw()
			case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A':
				if cur > 0 {
					cur--
				}
			case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B':
				if cur < len(filtered)-1 {
					cur++
				}
			case k[0] == 'k':
				if cur > 0 {
					cur--
				}
			case k[0] == 'j':
				if cur < len(filtered)-1 {
					cur++
				}
			default:
				continue
			}
			redraw()
			continue
		}

		switch {
		case k[0] == '\r' || k[0] == '\n':
			var out []int
			for i, c := range checked {
				if c {
					out = append(out, i)
				}
			}
			if len(out) == 0 {
				continue // need at least one selection
			}
			fmt.Fprint(w, "\r\n")
			return out, nil
		case k[0] == 3 || k[0] == 'q':
			fmt.Fprint(w, "\r\n")
			return nil, errCancelled
		case k[0] == '/': // enter search mode
			searching = true
			searchQuery = ""
			redraw()
		case k[0] == ' ':
			if len(filtered) > 0 {
				origIdx := filterIdx[cur]
				checked[origIdx] = !checked[origIdx]
			}
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A':
			if cur > 0 {
				cur--
			}
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B':
			if cur < len(filtered)-1 {
				cur++
			}
		case k[0] == 'k':
			if cur > 0 {
				cur--
			}
		case k[0] == 'j':
			if cur < len(filtered)-1 {
				cur++
			}
		default:
			continue
		}
		redraw()
	}
}

// filterIndices 返回匹配查询字符串的菜单项在原始列表中的索引。
func filterIndices(items []menuItem, query string) []int {
	if query == "" {
		out := make([]int, len(items))
		for i := range items {
			out[i] = i
		}
		return out
	}
	lq := strings.ToLower(query)
	var out []int
	for i, it := range items {
		if strings.Contains(strings.ToLower(it.name), lq) || strings.Contains(strings.ToLower(it.desc), lq) {
			out = append(out, i)
		}
	}
	return out
}

// FrameLines 为测试而导出，返回在给定状态下 selectOne/selectMany 将打印的终端总行数。
func FrameLines(filteredLen, termRows int, searching bool) int {
	return fixedLines(searching) + maxViewport(filteredLen, termRows, searching)
}
