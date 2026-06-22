// Package main 是 Reasonix CLI 的入口点。
// Reasonix 是一个配置驱动、插件驱动的编码智能体 CLI 工具。
//
// 本文件通过 blank import（空白导入）机制触发各子系统的自注册（init() 模式），
// 使得提供者（provider）和工具（tool）在编译时自动注册到各自的注册表中，
// 而无需在 main 中显式调用注册函数。
//
// 启动流程：
//  1. blank import 触发 provider/anthropic、provider/openai、tool/builtin 的 init()
//  2. main() 将命令行参数和版本号传递给 cli.Run()
//  3. cli.Run() 负责子命令路由、标志解析、从配置组装控制器
//  4. 进程退出码由 cli.Run() 返回
package main

import (
	"os"

	"reasonix/internal/cli"

	// blank import：注册 Anthropic 兼容的模型提供者（触发其 init() 函数）
	_ "reasonix/internal/provider/anthropic"
	// blank import：注册 OpenAI 兼容的模型提供者（触发其 init() 函数）
	_ "reasonix/internal/provider/openai"
	// blank import：注册内置工具集，如 write_file、edit_file、bash 等（触发其 init() 函数）
	_ "reasonix/internal/tool/builtin"
)

// version 是应用程序版本号。
// 在构建时通过 -ldflags "-X main.version=v1.2.3" 注入，
// 默认值 "dev" 表示开发构建（未指定版本号）。
var version = "dev"

// main 是 CLI 入口函数。
// 将 os.Args[1:]（跳过程序名）和版本号传递给 cli.Run()，
// 并将其返回的进程退出码传递给 os.Exit()。
func main() {
	os.Exit(cli.Run(os.Args[1:], version))
}
