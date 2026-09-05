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

## 生成 bridge.db

从代理列表生成桥配置：

```powershell
go run ./cmd/proxy-db `
  -proxy-file D:\work\data-1788313833471.csv `
  -output D:\work\server-bridge-direct\bridge.db `
  -bridge-port-start 10000
```

输入支持 TXT（每行一个代理）和 CSV（第一列为完整代理地址），输出格式为：

```text
10000,198.51.100.10:1080
10001,198.51.100.11:8080
```

工具会同时生成配套的 `bridge.csv`，默认与 `bridge.db` 同目录同名；CSV 列为 `bridgePort,proxyScheme,proxyAddr,username,password`。也可以用 `-csv-output` 指定路径。`-dedupe` 可按代理 `host:port` 去重；`-verbose` 打印每条生成记录。生成前应停止正在使用目标 `bridge.db` 的 bridge-direct 进程。

## 代理列表格式

批量工具统一支持以下代理格式：

- TXT：每行一个代理 URL，例如 `socks5://user:password@198.51.100.10:1080`
- CSV：第一列为完整代理 URL，首行为 `proxy`、`proxy_url` 或包含“代理”的表头时自动跳过
- 不带 scheme 的 `host:port` 默认按 SOCKS5 处理

`proxy-db` 和 `proxy-e2e` 还支持 `socks5h://`、`http://`；`socks5h://` 由 SOCKS5 代理解析目标域名。
`test_socks5_proxies.py` 专门测试 SOCKS5/SOCKS5H，不测试 HTTP 代理。

## 从 bridge11 格式生成代理 CSV

如果源文件每行格式为 `host:port:user:password`，可以使用：

```powershell
python .\scripts\convert_bridge11.py `
  --input D:\work\bridge11.csv `
  --output D:\work\bridge11-proxy.csv
```

输出为 `proxy` 单列 CSV，每行是完整的 SOCKS5 URL：

```csv
"proxy"
"socks5://user:password@host:port"
```

用户名和密码中的 `@`、`:`、`/`、空格等特殊字符会自动 URL 编码。默认输入和输出分别是
`D:\work\bridge11.csv` 与 `D:\work\bridge11-proxy.csv`；遇到非法行时默认终止转换，
需要忽略非法行时增加 `--skip-invalid`。

## 批量代理 bridge e2e 测试

`cmd/proxy-e2e` 每轮执行以下流程：读取代理列表，并发调用 `/bridge/add`，并发请求
`http://myip.ipipv.com/`，检查返回 JSON 的 `Ip` 字段和同一代理多次请求的出口 IP 一致性，
最后并发调用 `/bridge/del` 并检查 bridge 端口已关闭。`-concurrency`（简写 `-c`）同时限制
add、request、del 三个阶段的 worker 数；每个任务携带自己的代理索引和 bridge 端口，结果按任务
回写，不会因为并发丢失代理对应关系。`-requests-per-proxy` 会为每个代理生成多个请求任务，
同一个代理的请求不保证同时启动。

### 本地 bridge（默认）

不指定 `-bridge-url` 时，工具自动构建并启动当前模块的临时 bridge：

```powershell
go run ./cmd/proxy-e2e `
  -proxy-file D:\work\bridge11-proxy.csv `
  -rounds 1 `
  -concurrency 10 `
  -requests-per-proxy 2 `
  -report D:\work\proxy-e2e-report.json
```

如果已有 bridge-direct 二进制，可以通过 `-bridge-bin` 指定，跳过自动构建：

```powershell
go run ./cmd/proxy-e2e `
  -proxy-file D:\work\bridge11-proxy.csv `
  -bridge-bin D:\work\server-bridge-direct\bin\bridge-direct.exe
```

### 远程 bridge

指定 `-bridge-url` 后，工具不会启动或停止本地 bridge，而是使用 AES 加密调用远程管理 API。
`-bridge-key` 必须与远程 bridge 配置中的 `key` 完全一致；`-bridge-port-start` 必须指定，
工具会按代理顺序分配连续的远程监听端口。

```powershell
go run ./cmd/proxy-e2e `
  -proxy-file D:\work\bridge11-proxy.csv `
  -bridge-url http://10.0.0.8:5678 `
  -bridge-key '远程配置中的key' `
  -bridge-port-start 30000 `
  -rounds 1 `
  -concurrency 10 `
  -requests-per-proxy 2 `
  -report D:\work\proxy-e2e-remote-report.json
```

默认情况下 bridge 数据端口主机从 `-bridge-url` 推导。只有管理 API 和数据端口通过不同主机、
NAT、反向代理或隧道暴露时，才需要额外指定 `-bridge-host`：

```powershell
  -bridge-host 10.0.0.8
```

远程模式要求管理 API 和所有 bridge 数据端口都能从测试机访问；每轮只删除本次成功添加的
远程 bridge，避免端口冲突时误删其他任务的桥。

主要参数：

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `-proxy-file` | 必填 | TXT 或 CSV 代理列表 |
| `-rounds` | `1` | 测试轮数 |
| `-concurrency`, `-c` | `10` | add/request/del 三个阶段的最大并发数 |
| `-requests-per-proxy` | `2` | 每个代理每轮请求任务数 |
| `-request-timeout` | `25s` | 单次 HTTP 请求超时 |
| `-bridge-bin` | 自动构建 | 本地模式使用的 bridge 二进制 |
| `-bridge-url` | 空 | 远程 bridge 管理 API，设置后启用远程模式 |
| `-bridge-key` | 空 | 远程 bridge AES key，远程模式必填 |
| `-bridge-host` | API 主机 | bridge 数据端口可访问的主机，可选覆盖 |
| `-bridge-port-start` | `0` | 远程模式必填的第一个 bridge 端口 |
| `-report` | 空 | JSON 明细报告路径，不保存代理密码 |
| `-verbose` | `false` | 输出每个代理的 add/request/del 明细 |
| `-dry-run` | `false` | 只解析代理文件，不启动 bridge 或发送请求 |

也可以使用 PowerShell 包装脚本：

```powershell
.\scripts\proxy-e2e.ps1 `
  -ProxyFile D:\work\bridge11-proxy.csv `
  -Rounds 1 `
  -Concurrency 10 `
  -RequestsPerProxy 2 `
  -Report D:\work\proxy-e2e-report.json
```

### Python SOCKS5 代理连通性测试

`scripts/test_socks5_proxies.py` 使用 Python 标准库逐个测试 SOCKS5 代理 TCP 连接、用户名密码
认证，以及访问 `http://myip.ipipv.com/`。它支持 TXT 或 CSV 第一列代理地址，默认并发数为 10：

```powershell
python .\scripts\test_socks5_proxies.py `
  --proxy-file D:\work\bridge11-proxy.csv `
  --concurrency 10 `
  --requests-per-proxy 3 `
  --timeout 15 `
  --report D:\work\proxy-check-report.json
```

`--requests-per-proxy` 指定每个代理的独立测试次数，默认为 `1`；同一个代理的多次测试也会
作为独立任务参与全局并发调度。输出会分类统计 `proxy_connect_failed`、`auth_failed`、`timeout`、`target_request_failed`、
`target_response_invalid` 和 `success`；失败进度行和最终汇总后的失败明细都会打印具体的 `reason` 及出现次数。报告只保存脱敏后的
主机端口、认证状态、失败原因和出口 IP，不会保存用户名或密码；全部成功时退出码为 `0`，
存在失败时退出码为 `1`。

查看任一工具的完整参数说明：

```powershell
go run ./cmd/proxy-db -h
go run ./cmd/proxy-e2e -h
python .\scripts\convert_bridge11.py --help
python .\scripts\test_socks5_proxies.py --help
```
