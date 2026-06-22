// latex.go 实现了 LaTeX 数学表达式到 Unicode 字符的转换。
// 该文件负责：
//   - 将 LaTeX 数学符号（如 \alpha、\sum、\infty）映射为 Unicode 字符
//   - 处理上标/下标（^、_）转换为 Unicode 上下标字符
//   - 渲染分数（\frac）、根号（\sqrt）和重音符号
//   - 将 \(..\) 和 \[..\] 等替代数学分隔符标准化为 $..$ / $$..$$
//   - 实现黑板粗体（\mathbb）等数学字体转换
// 所有符号映射表均为手工维护，无第三方 Go 库依赖。
package cli

import (
	"strings"
	"unicode/utf8"
)

// latexToUnicode 将 LaTeX 数学表达式转换为 Unicode 字符近似表示。
// 支持希腊字母、运算符、关系符、上下标、分数、根号和重音符号等；
// 无法表示的内容会降级为可读的纯文本形式。
func latexToUnicode(expr string) string {
	return convertMath([]rune(expr))
}

// convertMath 是 LaTeX 数学表达式转换的核心递归函数。
// 逐字符扫描输入，处理反斜杠命令、上标(^)、下标(_)、花括号和其他特殊字符。
func convertMath(rs []rune) string {
	var b strings.Builder
	b.Grow(len(rs))
	for i := 0; i < len(rs); {
		switch r := rs[i]; r {
		case '\\':
			i = convertCommand(&b, rs, i)
		case '^':
			arg, ni := readAtom(rs, i+1, false)
			b.WriteString(superscript(convertMath([]rune(arg))))
			i = ni
		case '_':
			arg, ni := readAtom(rs, i+1, false)
			b.WriteString(subscript(convertMath([]rune(arg))))
			i = ni
		case '{', '}':
			i++
		case '&', '~':
			b.WriteByte(' ')
			i++
		case '$':
			i++
		default:
			b.WriteRune(r)
			i++
		}
	}
	return b.String()
}

// convertCommand consumes the backslash command starting at rs[i] and writes
// its rendering; it returns the index just past everything it consumed.
func convertCommand(b *strings.Builder, rs []rune, i int) int {
	j := i + 1
	if j >= len(rs) {
		return j
	}
	if !isASCIILetter(rs[j]) {
		switch ch := rs[j]; ch {
		case '\\':
			b.WriteString("  ")
		case ',', ';', ':', '!', ' ':
			b.WriteByte(' ')
		default:
			b.WriteRune(ch)
		}
		return j + 1
	}

	k := j
	for k < len(rs) && isASCIILetter(rs[k]) {
		k++
	}
	cmd := string(rs[j:k])

	switch cmd {
	case "frac", "tfrac", "dfrac":
		num, k2 := readAtom(rs, k, true)
		den, k3 := readAtom(rs, k2, true)
		b.WriteString(renderFrac(convertMath([]rune(num)), convertMath([]rune(den))))
		return k3
	case "sqrt":
		idx := ""
		if k < len(rs) && rs[k] == '[' {
			idx, k = readBracket(rs, k)
		}
		arg, k2 := readAtom(rs, k, true)
		b.WriteString(renderSqrt(idx, convertMath([]rune(arg))))
		return k2
	case "text", "textrm", "textbf", "textit", "mathrm", "mathsf", "mathtt", "mathit", "mathbf", "mathcal", "operatorname":
		arg, k2 := readAtom(rs, k, true)
		b.WriteString(arg)
		return k2
	case "mathbb":
		arg, k2 := readAtom(rs, k, true)
		b.WriteString(blackboard(arg))
		return k2
	case "left", "right", "big", "Big", "bigg", "Bigg", "bigl", "bigr", "Bigl", "Bigr", "displaystyle", "textstyle", "limits", "nolimits":
		return k
	case "begin", "end":
		_, k2 := readAtom(rs, k, true)
		return k2
	}

	if combining, ok := accents[cmd]; ok {
		arg, k2 := readAtom(rs, k, true)
		b.WriteString(applyCombining(convertMath([]rune(arg)), combining))
		return k2
	}
	if sym, ok := symbols[cmd]; ok {
		b.WriteString(sym)
		return k
	}
	b.WriteString(cmd)
	return k
}

// readAtom reads the argument of a command or script: a {balanced group}, a
// \command, or a single rune. Returns the inner text (no surrounding braces)
// and the index just past it.
// readAtom 读取命令或脚标的参数：可以是花括号包围的分组、一个 \命令或单个字符。
// 返回参数的内部文本（不含外层花括号）和消费后的位置索引。
func readAtom(rs []rune, i int, skipSpaces bool) (string, int) {
	if skipSpaces {
		for i < len(rs) && rs[i] == ' ' {
			i++
		}
	}
	if i >= len(rs) {
		return "", i
	}
	switch rs[i] {
	case '{':
		depth := 0
		start := i + 1
		for j := i; j < len(rs); j++ {
			switch rs[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return string(rs[start:j]), j + 1
				}
			}
		}
		return string(rs[start:]), len(rs)
	case '\\':
		k := i + 1
		if k < len(rs) && !isASCIILetter(rs[k]) {
			return string(rs[i : k+1]), k + 1
		}
		for k < len(rs) && isASCIILetter(rs[k]) {
			k++
		}
		return string(rs[i:k]), k
	default:
		return string(rs[i]), i + 1
	}
}

// readBracket 读取方括号 [...] 中的内容，用于 \sqrt[n]{x} 中的可选参数。
func readBracket(rs []rune, i int) (string, int) {
	start := i + 1
	for j := start; j < len(rs); j++ {
		if rs[j] == ']' {
			return string(rs[start:j]), j + 1
		}
	}
	return "", len(rs)
}

// renderFrac 将分数渲染为 "numerator/denominator" 形式，
// 多字符的分子或分母会用括号包裹以保持可读性。
func renderFrac(num, den string) string {
	return wrapIfCompound(num) + "/" + wrapIfCompound(den)
}

// renderSqrt 渲染根号表达式，支持二次根号(√)、三次根号(∛)和四次根号(∜)。
// 多字符的被开方数会用括号包裹。
func renderSqrt(idx, arg string) string {
	if utf8.RuneCountInString(arg) > 1 {
		arg = "(" + arg + ")"
	}
	switch idx {
	case "", "2":
		return "√" + arg
	case "3":
		return "∛" + arg
	case "4":
		return "∜" + arg
	}
	return superscript(idx) + "√" + arg
}

// wrapIfCompound 当字符串包含多个字符时用括号包裹，用于分数的分子/分母显示。
func wrapIfCompound(s string) string {
	if utf8.RuneCountInString(s) > 1 {
		return "(" + s + ")"
	}
	return s
}

// applyCombining 在字符串的第一个字符后插入组合用重音符号（如 hat、bar、dot 等）。
func applyCombining(s string, mark rune) string {
	rs := []rune(s)
	if len(rs) == 0 {
		return string(mark)
	}
	return string(rs[0]) + string(mark) + string(rs[1:])
}

// blackboard 将字符串中的大写字母转换为黑板粗体（如 R → ℝ, N → ℕ），
// 用于渲染 \mathbb{R} 等数学符号。
func blackboard(s string) string {
	var b strings.Builder
	for _, r := range s {
		if bb, ok := blackboardCaps[r]; ok {
			b.WriteRune(bb)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// superscript 将字符串转换为 Unicode 上标形式。
// 如果所有字符都有对应的上标映射则直接替换，否则使用 ^ 前缀标记。
func superscript(s string) string {
	if t, ok := mapAll(s, superMap); ok {
		return t
	}
	if utf8.RuneCountInString(s) == 1 {
		return "^" + s
	}
	return "^(" + s + ")"
}

// subscript 将字符串转换为 Unicode 下标形式。
// 如果所有字符都有对应的下标映射则直接替换，否则使用 _ 前缀标记。
func subscript(s string) string {
	if t, ok := mapAll(s, subMap); ok {
		return t
	}
	if utf8.RuneCountInString(s) == 1 {
		return "_" + s
	}
	return "_(" + s + ")"
}

// mapAll 尝试将字符串中的所有字符通过给定映射表转换。
// 只有当所有字符都能映射时才返回成功。
func mapAll(s string, m map[rune]rune) (string, bool) {
	if s == "" {
		return "", true
	}
	var b strings.Builder
	for _, r := range s {
		c, ok := m[r]
		if !ok {
			return "", false
		}
		b.WriteRune(c)
	}
	return b.String(), true
}

// isASCIILetter 判断字符是否为 ASCII 字母（a-z 或 A-Z）。
func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// normalizeMath rewrites the alternate math delimiters \(..\) and \[..\] to
// $..$ / $$..$$ and collapses newlines inside a $$ display block onto one line
// so the inline math parser sees a single contiguous run. It tracks fenced and
// inline code so literal delimiters inside code are never rewritten.
func normalizeMath(s string) string {
	rs := []rune(s)
	n := len(rs)
	var b strings.Builder
	b.Grow(len(s))

	inFenced, inCode, inDisplay := false, false, false

	for i := 0; i < n; {
		r := rs[i]

		if r == '`' && i+2 < n && rs[i+1] == '`' && rs[i+2] == '`' {
			inFenced = !inFenced
			b.WriteString("```")
			i += 3
			continue
		}
		if r == '`' && !inFenced {
			inCode = !inCode
			b.WriteRune(r)
			i++
			continue
		}
		if inFenced || inCode {
			b.WriteRune(r)
			i++
			continue
		}

		if r == '\\' && i+1 < n {
			switch rs[i+1] {
			case '\\':
				b.WriteString("\\\\")
				i += 2
				continue
			case '[':
				b.WriteString("$$")
				inDisplay = true
				i += 2
				continue
			case ']':
				b.WriteString("$$")
				inDisplay = false
				i += 2
				continue
			case '(':
				b.WriteString("$")
				i += 2
				continue
			case ')':
				b.WriteString("$")
				i += 2
				continue
			}
		}
		if r == '$' && i+1 < n && rs[i+1] == '$' {
			b.WriteString("$$")
			inDisplay = !inDisplay
			i += 2
			continue
		}
		if r == '\n' && inDisplay {
			b.WriteByte(' ')
			i++
			continue
		}

		b.WriteRune(r)
		i++
	}
	return b.String()
}

// symbols 是 LaTeX 命令名到 Unicode 字符的映射表，
// 涵盖希腊字母、运算符、关系符、箭头、积分、集合论等常见数学符号。
var symbols = map[string]string{
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ε",
	"varepsilon": "ε", "zeta": "ζ", "eta": "η", "theta": "θ", "vartheta": "ϑ",
	"iota": "ι", "kappa": "κ", "lambda": "λ", "mu": "μ", "nu": "ν", "xi": "ξ",
	"omicron": "ο", "pi": "π", "varpi": "ϖ", "rho": "ρ", "varrho": "ϱ",
	"sigma": "σ", "varsigma": "ς", "tau": "τ", "upsilon": "υ", "phi": "φ",
	"varphi": "ϕ", "chi": "χ", "psi": "ψ", "omega": "ω",
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ",
	"Pi": "Π", "Sigma": "Σ", "Upsilon": "Υ", "Phi": "Φ", "Psi": "Ψ", "Omega": "Ω",

	"times": "×", "div": "÷", "cdot": "·", "ast": "∗", "star": "⋆",
	"pm": "±", "mp": "∓", "oplus": "⊕", "ominus": "⊖", "otimes": "⊗",
	"oslash": "⊘", "odot": "⊙", "circ": "∘", "bullet": "•", "setminus": "∖",

	"leq": "≤", "le": "≤", "geq": "≥", "ge": "≥", "neq": "≠", "ne": "≠",
	"equiv": "≡", "approx": "≈", "cong": "≅", "sim": "∼", "simeq": "≃",
	"propto": "∝", "ll": "≪", "gg": "≫", "doteq": "≐", "asymp": "≍",

	"leftarrow": "←", "rightarrow": "→", "to": "→", "gets": "←",
	"leftrightarrow": "↔", "Leftarrow": "⇐", "Rightarrow": "⇒",
	"Leftrightarrow": "⇔", "implies": "⇒", "iff": "⇔", "mapsto": "↦",
	"uparrow": "↑", "downarrow": "↓", "longrightarrow": "⟶", "longleftarrow": "⟵",

	"sum": "∑", "prod": "∏", "coprod": "∐", "int": "∫", "iint": "∬",
	"iiint": "∭", "oint": "∮", "nabla": "∇", "partial": "∂",
	"infty": "∞", "sqrt": "√", "surd": "√",

	"in": "∈", "notin": "∉", "ni": "∋", "subset": "⊂", "supset": "⊃",
	"subseteq": "⊆", "supseteq": "⊇", "cup": "∪", "cap": "∩",
	"emptyset": "∅", "varnothing": "∅", "forall": "∀", "exists": "∃",
	"nexists": "∄", "neg": "¬", "lnot": "¬", "land": "∧", "wedge": "∧",
	"lor": "∨", "vee": "∨",

	"angle": "∠", "perp": "⊥", "parallel": "∥", "mid": "∣", "nmid": "∤",
	"triangle": "△", "square": "□", "diamond": "◇", "top": "⊤", "bot": "⊥",
	"vdash": "⊢", "models": "⊨", "therefore": "∴", "because": "∵",

	"ldots": "…", "dots": "…", "cdots": "⋯", "vdots": "⋮", "ddots": "⋱",
	"prime": "′", "degree": "°", "deg": "°", "hbar": "ℏ", "ell": "ℓ",
	"Re": "ℜ", "Im": "ℑ", "aleph": "ℵ", "wp": "℘",
	"langle": "⟨", "rangle": "⟩", "lceil": "⌈", "rceil": "⌉",
	"lfloor": "⌊", "rfloor": "⌋", "backslash": "\\",

	"quad": "  ", "qquad": "    ", "space": " ", "thinspace": " ",
	"lim": "lim", "sin": "sin", "cos": "cos", "tan": "tan", "log": "log",
	"ln": "ln", "exp": "exp", "min": "min", "max": "max", "det": "det",
	"gcd": "gcd", "dim": "dim", "ker": "ker",
}

// accents 是 LaTeX 重音命令到 Unicode 组合用字符的映射表。
var accents = map[string]rune{
	"hat": '̂', "widehat": '̂', "bar": '̄', "overline": '̄',
	"vec": '⃗', "dot": '̇', "ddot": '̈', "tilde": '̃',
	"widetilde": '̃', "acute": '́', "grave": '̀', "check": '̌',
}

// superMap 是普通字符到 Unicode 上标字符的映射表。
var superMap = map[rune]rune{
	'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶',
	'7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽',
	')': '⁾', 'a': 'ᵃ', 'b': 'ᵇ', 'c': 'ᶜ', 'd': 'ᵈ', 'e': 'ᵉ', 'f': 'ᶠ',
	'g': 'ᵍ', 'h': 'ʰ', 'i': 'ⁱ', 'j': 'ʲ', 'k': 'ᵏ', 'l': 'ˡ', 'm': 'ᵐ',
	'n': 'ⁿ', 'o': 'ᵒ', 'p': 'ᵖ', 'r': 'ʳ', 's': 'ˢ', 't': 'ᵗ', 'u': 'ᵘ',
	'v': 'ᵛ', 'w': 'ʷ', 'x': 'ˣ', 'y': 'ʸ', 'z': 'ᶻ',
}

// subMap 是普通字符到 Unicode 下标字符的映射表。
var subMap = map[rune]rune{
	'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆',
	'7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍',
	')': '₎', 'a': 'ₐ', 'e': 'ₑ', 'h': 'ₕ', 'i': 'ᵢ', 'j': 'ⱼ', 'k': 'ₖ',
	'l': 'ₗ', 'm': 'ₘ', 'n': 'ₙ', 'o': 'ₒ', 'p': 'ₚ', 'r': 'ᵣ', 's': 'ₛ',
	't': 'ₜ', 'u': 'ᵤ', 'v': 'ᵥ', 'x': 'ₓ',
}

// blackboardCaps 是大写字母到黑板粗体 Unicode 字符的映射表（如 A → 𝔸, R → ℝ）。
var blackboardCaps = map[rune]rune{
	'A': '𝔸', 'B': '𝔹', 'C': 'ℂ', 'D': '𝔻', 'E': '𝔼', 'F': '𝔽', 'G': '𝔾',
	'H': 'ℍ', 'I': '𝕀', 'J': '𝕁', 'K': '𝕂', 'L': '𝕃', 'M': '𝕄', 'N': 'ℕ',
	'O': '𝕆', 'P': 'ℙ', 'Q': 'ℚ', 'R': 'ℝ', 'S': '𝕊', 'T': '𝕋', 'U': '𝕌',
	'V': '𝕍', 'W': '𝕎', 'X': '𝕏', 'Y': '𝕐', 'Z': 'ℤ',
}
