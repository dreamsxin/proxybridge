# bridge-direct

## 构建

项目提供两个构建脚本。脚本每次执行时自动生成当前 UTC 时间作为 `BuildTime`；如果系统存在 Git 且当前目录属于仓库，则自动读取当前 HEAD 的短提交号，否则注入 `unknown`，不会因为 Git 不存在而构建失败。`VERSION` 是源码中的产品版本常量，不通过 linker 修改。

### Windows: `build.ps1`

在仓库根目录的 PowerShell 中执行：

```powershell
.\build.ps1
```

输出文件：

```text
bin/bridge-direct.exe       Windows amd64
bin/bridge-direct           Linux amd64
bin/bridge-direct-arm       Linux arm64
```

脚本使用以下编译参数：

```text
-ldflags "-w -s -X main.BuildTime=<UTC时间> -X main.GitCommit=<Git短提交号>"
```

其中 `-w -s` 用于去除调试信息和符号表；`BuildTime` 和 `GitCommit` 不需要用户传入，由脚本自动生成；脚本会临时设置 `CGO_ENABLED=0`、`GOOS` 和 `GOARCH`，结束后恢复调用 PowerShell 会话中的环境变量。

### Linux/macOS: `build.sh`

在仓库根目录执行：

```sh
sh ./build.sh
```

输出文件：

```text
bin/bridge-direct-arm       Linux arm64
bin/bridge-direct           Linux amd64
```

脚本使用相同的 linker 参数，并设置：

```text
CGO_ENABLED=0
GOOS=linux
GOARCH=arm64 或 amd64
```

### 手工构建

如需绕过脚本手工构建，也可以显式注入构建时间和提交号：

```powershell
go build -ldflags "-X main.BuildTime=2026-09-03T08:00:00Z -X main.GitCommit=abc1234" -o bin/bridge-direct.exe main.go
```

未通过 linker 注入时：

- `VERSION` 使用源码常量
- `BuildTime` 使用程序启动时的 UTC 时间
- `GitCommit` 从 Go build info 读取；没有 VCS 信息时为 `unknown`
- 程序运行时不会调用 Git 命令

查看版本：

```powershell
bin\bridge-direct.exe version
```
