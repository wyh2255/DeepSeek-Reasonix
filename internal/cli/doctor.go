// doctor.go 实现了 "reasonix doctor" 命令的 CLI 入口。
// 该命令用于诊断当前环境的配置状态和运行条件，
// 支持纯文本和 JSON 两种输出格式。

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"reasonix/internal/doctor"
)

// doctorCommand 处理 "reasonix doctor" 命令，收集并展示环境诊断信息。
// 支持 --json 标志以 JSON 格式输出诊断报告。
// 诊断内容由 doctor.Collect 收集，doctor.RenderText 渲染为可读文本。
func doctorCommand(args []string, version string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print diagnostics as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	report := doctor.Collect(doctor.Options{Version: version})
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	fmt.Print(doctor.RenderText(report))
	return 0
}
