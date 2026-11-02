// Command sandbox-runner 安装在 SSH 远端，用远端的本机隔离后端执行命令。用法见 sandbox/sshsandbox。
package main

import (
	"os"

	"github.com/kart-io/goagent-harness/sandbox/sshsandbox"
)

func main() {
	os.Exit(sshsandbox.RunnerMain(os.Args[1:], os.Stdout, os.Stderr))
}
