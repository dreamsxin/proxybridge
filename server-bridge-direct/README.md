# bridge-direct

运行模式说明：`local` 模式跳过中心 `syncBridge`，但仍启动本地管理 API；`remote` 模式在启动管理 API
前执行中心同步。两种模式都支持 `/bridge/status`、`/bridge/start`、`/bridge/add` 和 `/bridge/del`。

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

## Go 命令索引

| 命令 | 用途 |
|---|---|
| `go run . -c config.json` | 启动 bridge-direct 服务；启动时会在控制台打印管理端口和指标端点状态 |
| `go run . version` | 打印产品版本、构建时间和 Git commit |
| `go run ./cmd/bridge-info -h` | 从 `/metrics` 或 `/bridge/status` 打印运维指标摘要 |
| `go run ./cmd/proxy-db -h` | 查看代理列表生成 `bridge.db`/`bridge.csv` 的参数 |
| `go run ./cmd/proxy-e2e -h` | 查看本地或远程 bridge 批量 e2e 测试参数 |
| `go test ./...` | 运行全部普通 Go 单元测试 |
| `go test -tags e2e ./e2e -v` | 启动真实进程和 SOCKS5 测试服务，运行代码级 e2e |

启动服务示例：

```powershell
go run . -c config.json
```

控制台会先打印类似信息：

```text
bridge-direct startup version=v0.0.6+20260901 build_time=... git_commit=...
bridge-direct management listen=:5678
bridge-direct metrics listen=127.0.0.1:9090 path=/metrics
```

未配置 `metricsAddr` 时会明确打印：

```text
bridge-direct metrics=disabled
```

`cmd/proxy-db` 用于生成桥缓存，详见“生成 bridge.db”；`cmd/proxy-e2e` 用于 add/request/del
批量验证，详见“批量代理 bridge e2e 测试”。所有命令都支持 `-h` 查看当前编译版本的完整参数。

运维指标摘要命令示例：

```powershell
go run ./cmd/bridge-info `
  -addr http://127.0.0.1:5678 `
  -metrics-url http://127.0.0.1:9090/metrics
```

`bridge-info` 的 `-source` 默认为 `auto`：优先读取 Prometheus 指标，失败时自动回退到
`-addr/bridge/status`。也可以强制指定来源：

```powershell
go run ./cmd/bridge-info -source status -addr http://127.0.0.1:5678 -check
go run ./cmd/bridge-info -source metrics -metrics-url http://127.0.0.1:9090/metrics
```

常用参数：

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `-addr` | `http://127.0.0.1:5678` | 管理 API 基础地址，用于 `/bridge/status` |
| `-metrics-url` | `<addr>/metrics` | Prometheus 指标地址 |
| `-source` | `auto` | `auto`、`metrics` 或 `status` |
| `-bridge-port` | `0` | status 模式只查询指定端口；0 表示全部 |
| `-check` | `false` | status 模式执行 bridge/proxy TCP 探测 |
| `-timeout` | `5s` | HTTP 请求超时 |
| `-watch` | `0` | 按指定间隔重复输出；0 表示只输出一次 |
| `-json` | `false` | 输出 JSON 摘要，适合脚本采集 |

## 配置文件说明

服务通过 `-c` 指定 JSON 配置文件：

```powershell
go run . -c config.json
```

下面是一个完整示例。示例使用 `local` 模式，不从中心同步；管理 API 监听 `:5678`，Prometheus
指标监听 `127.0.0.1:9090`：

```json
{
  "mode": "local",
  "addr": ":5678",
  "syncDomain": "http://192.168.0.250:7888",
  "key": "abcd1234poiu5678bvbvnbnb",
  "dataFilename": "bridge.db",
  "bridgeId": 12314,
  "logFile": "logs/bridge.log",
  "logConsole": false,
  "logLevel": "info",
  "logFormat": "text",
  "logSource": false,
  "logMaxSizeMB": 100,
  "logMaxAgeDays": 7,
  "logMaxBackups": 10,
  "logCompress": true,
  "pprofAddr": "",
  "metricsAddr": "127.0.0.1:9090",
  "statsInterval": 60,
  "connIdleTimeout": 0,
  "maxConnsPerPort": 0,
  "maxConns": 0
}
```

### 运行和日志配置

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `mode` | 空字符串 | `local`：跳过 `syncBridge`；`remote`：启动时从中心同步。只有精确等于 `local` 才是本地模式，其他值按 remote 路径处理。两种模式都会启动管理 API。 |
| `addr` | `:8080` | 管理 API 监听地址，提供 `/bridge/status`、`/bridge/start`、`/bridge/add`、`/bridge/del`。 |
| `syncDomain` | 空 | 中心同步服务基础地址；remote 模式请求其 `/api/notify/bridges`。同步失败会记录错误并继续使用本地缓存。 |
| `key` | 无 | 管理请求 AES-CBC 密钥，必须是 16/24/32 字节；缺失或长度错误时进程启动失败。不要提交真实密钥。 |
| `dataFilename` | 无 | 本地 `bridge.db` 路径；必须可创建或读取，否则进程启动失败。 |
| `bridgeId` | `0` | remote 模式发给中心的桥 ID。local 模式不参与同步。 |
| `logFile` | 空 | 日志文件路径。为空时日志直接输出 stdout；设置后使用 lumberjack 轮转。 |
| `logConsole` | `false` | `logFile` 非空时是否同时复制日志到 stdout。服务版本、管理地址和指标地址仍会单独打印到 stdout。 |
| `logLevel` | `info` | `debug`、`info`、`warn`、`error`。生产建议 `info`；`debug` 会为转发流量输出更多日志。 |
| `logFormat` | `text` | `text` 或 `json`。JSON 便于日志采集系统解析。 |
| `logSource` | `false` | 是否在日志中附加源码文件和行号。开启会增加日志体积。 |
| `logMaxSizeMB` | `100` | 单个日志文件达到该大小后轮转；小于等于 0 会回退到 100。 |
| `logMaxAgeDays` | `7` | 归档日志最长保留天数；小于等于 0 回退到 7。 |
| `logMaxBackups` | `10` | 最多保留的归档文件数；小于等于 0 回退到 10。 |
| `logCompress` | `true` | 是否压缩归档日志；缺失时默认开启，显式 `false` 才关闭。 |

### 调试、监控和连接限制配置

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `pprofAddr` | 空（关闭） | Go pprof 监听地址，例如 `127.0.0.1:6060`。无认证，只建议绑定回环或监控内网。 |
| `metricsAddr` | 空（关闭） | Prometheus `/metrics` 监听地址，例如 `127.0.0.1:9090`。无认证，只建议绑定回环或监控内网。 |
| `statsInterval` | `0`（关闭） | 运行水位日志间隔，单位秒；建议排查泄漏时设置为 `60`。 |
| `connIdleTimeout` | `0`（不限制） | 单连接空闲超时，单位秒。SOCKS5 长连接、WebSocket 和连接池场景不建议设置过小。 |
| `maxConnsPerPort` | `0`（不限制） | 单个 bridge 端口最大活动连接数；超限时拒绝新连接。 |
| `maxConns` | `0`（不限制） | 进程级最大活动连接数；超限时拒绝新连接，是 fd 资源保护的主要上限。 |

配置优先级和启动行为：

- JSON 配置缺失字段使用上表默认值；日志轮转参数的非法非正值也会回退默认值。
- `local` 模式不调用中心同步，但仍会启动管理 API、恢复本地 `bridge.db` 中的 bridge。
- `remote` 模式先同步中心；同步失败时不会擦除本地缓存，而是记录错误并继续使用本地数据。
- `pprofAddr`、`metricsAddr` 都是独立监听器，端口不能与管理端口或 bridge 端口冲突。
- `key`、`dataFilename` 是运行必需项，没有安全的通用默认值，必须显式配置。
- 启动时始终向 stdout 打印版本、管理监听地址和指标启用状态，便于 systemd、容器或 Windows 服务确认实际配置。

## `/bridge/status` 返回值

`GET /bridge/status` 查询 bridge 运行状态和进程统计。该接口只允许回环地址访问，适合在
bridge 所在机器上通过脚本、监控代理或本机运维工具调用：

```powershell
Invoke-RestMethod 'http://127.0.0.1:5678/bridge/status'
Invoke-RestMethod 'http://127.0.0.1:5678/bridge/status?bridgePort=10000&check=1'
```

查询参数：

| 参数 | 必填 | 说明 |
|---|---|---|
| `bridgePort` | 否 | 只返回指定 bridge 端口；省略时返回全部运行中 bridge，以及缓存中配置存在但 listener 尚未运行的 bridge。 |
| `proxyAddr` | 否 | 按目标地址过滤，例如 `192.0.2.10:1080`。 |
| `check` | 否 | `1` 或 `true` 时额外探测 bridge 监听端口和目标代理 TCP 连通性；会产生网络连接和最多约 2 秒的探测耗时。 |

成功响应结构：

```json
{
  "code": 200,
  "msg": "ok",
  "data": [
    {
      "bridgePort": 10000,
      "proxyAddr": "192.0.2.10:1080",
      "listening": true,
      "bridgeTcp": true,
      "proxyTcp": true,
      "ok": true,
      "bindErr": "",
      "bridgeErr": "",
      "proxyErr": "",
      "failureReason": "",
      "solution": ""
    }
  ],
  "stats": {
    "goroutines": 12,
    "bridges": 1,
    "listening": 1,
    "conns": 0,
    "accepted": 42,
    "rejected": 0,
    "dialOK": 42,
    "dialFail": 0,
    "heapAllocMB": 8,
    "sysMB": 21,
    "numGC": 3
  }
}
```

`data` 单桥状态字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `bridgePort` | number | bridge 对外监听端口。 |
| `proxyAddr` | string | 当前目标代理地址，格式为 `ip:port`。 |
| `listening` | boolean | listener 是否已经成功绑定并处于监听状态。bind 重试期间为 `false`。 |
| `bridgeTcp` | boolean | `check=1` 时对本机 bridge 端口的 TCP 探测结果；未开启探测时为默认值 `false`。 |
| `proxyTcp` | boolean | `check=1` 时对目标代理地址的 TCP 探测结果；未开启探测时为默认值 `false`。 |
| `ok` | boolean | 只有 `listening=true`，且在 `check=1` 时 bridge/proxy TCP 探测均成功才为 `true`。 |
| `bindErr` | string | 最近一次 bind 失败原因；listener 正常绑定后为空。 |
| `bridgeErr` | string | bridge 端口 TCP 探测失败原因。 |
| `proxyErr` | string | 目标代理 TCP 探测失败原因。 |
| `failureReason` | string | 面向运维的失败原因汇总。 |
| `solution` | string | 面向运维的处理建议。 |

不带 `check` 时不会执行网络探测，因此 `bridgeTcp`、`proxyTcp`、`ok` 不能用于判断目标代理
当前是否可连接；这可以避免频繁轮询 status 时产生额外连接。需要连通性诊断时再使用
`check=1`，并控制调用频率。

`stats` 进程和 bridge 统计字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `goroutines` | number | 当前 Go goroutine 数量。持续增长可能表示 goroutine 泄漏或请求未收敛。 |
| `bridges` | number | 当前运行态 listener supervisor 数量，不包含仅存在于 bridge.db 但尚未启动的配置。 |
| `listening` | number | 当前实际完成 bind 的 listener 数量。小于 `bridges` 通常表示有端口处于 bind 重试。 |
| `conns` | number | 当前所有运行态 bridge 的活动转发连接数。 |
| `accepted` | number | 当前运行 listener 生命周期内累计接受的客户端连接数；删除 listener 后该 listener 的计数不再计入。 |
| `rejected` | number | 当前运行 listener 生命周期内因全局或单端口连接上限拒绝的连接数。 |
| `dialOK` | number | 当前运行 listener 生命周期内目标代理拨号成功数。 |
| `dialFail` | number | 当前运行 listener 生命周期内目标代理拨号失败数。 |
| `heapAllocMB` | number | Go 当前堆分配量，单位 MB。 |
| `sysMB` | number | Go 从操作系统获得的内存量，单位 MB。 |
| `numGC` | number | Go 垃圾回收完成次数。 |

`stats.accepted/rejected/dialOK/dialFail` 是当前运行 listener 的运行水位，不等同于 Prometheus
中进程生命周期累计的 `*_total` 指标。需要长期趋势、速率和告警时使用 Prometheus；需要一次性
查看当前 bridge 状态时使用 `/bridge/status`。

错误示例（HTTP 状态仍可能是 200，需检查 JSON 的 `code`）：

```json
{
  "code": 403,
  "msg": "bridge status is only available from localhost"
}
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

## Prometheus 指标

Prometheus 指标默认关闭。配置 `metricsAddr` 后，bridge-direct 会单独监听该地址的
`/metrics`，不会占用管理 API 端口：

```json
{
  "metricsAddr": "127.0.0.1:9090"
}
```

建议只绑定回环或监控内网地址；该端点没有额外认证。配置为空时不会打开指标监听器。

验证端点：

```powershell
(Invoke-WebRequest http://127.0.0.1:9090/metrics).Content
```

Prometheus 配置示例：

```yaml
scrape_configs:
  - job_name: bridge-direct
    static_configs:
      - targets: ["127.0.0.1:9090"]
```

指标分为汇总、事件流量、单桥和运行时四类：

| 类型 | 指标 | 类型/标签 |
|---|---|---|
| 汇总水位 | `bridge_proxies_configured`、`bridge_bridges`、`bridge_listeners`、`bridge_connections_active_total` | Gauge；配置桥数、运行 listener 数、监听数、当前活动连接数 |
| 连接事件 | `bridge_connections_accepted_total` | Counter；进程生命周期累计接受连接数 |
| 连接拒绝 | `bridge_connections_rejected_total{reason}` | Counter；`reason=global_limit` 或 `port_limit` |
| 目标拨号 | `bridge_dials_total{result}` | Counter；`result=success` 或 `failure` |
| 转发流量 | `bridge_relay_bytes_total{direction}` | Counter；`direction=up` 或 `down`，单位 bytes |
| Listener 错误 | `bridge_listener_errors_total{stage}` | Counter；`stage=bind` 或 `accept` |
| 单桥状态 | `bridge_listener_up{bridge_id,bridge_port,proxy_addr}` | Gauge；1=正在监听，0=配置存在但未监听 |
| 单桥连接 | `bridge_listener_connections_active{bridge_id,bridge_port,proxy_addr}` | Gauge；当前活动连接数 |
| 单桥事件 | `bridge_listener_connections_accepted_total{...}`、`bridge_listener_connections_rejected_total{...,reason}` | Counter；单桥接受/拒绝连接数 |
| 单桥拨号 | `bridge_listener_dials_total{...,result}` | Counter；单桥目标拨号成功/失败 |
| 单桥流量 | `bridge_listener_relay_bytes_total{...,direction}` | Counter；单桥上下行 bytes |
| 单桥错误 | `bridge_listener_errors_by_stage_total{...,stage}` | Counter；单桥 bind/accept 错误 |
| 拨号延迟 | `bridge_dial_duration_seconds{bridge_id,bridge_port,proxy_addr}` | Histogram；固定 10 桶，单位 seconds |
| 运行时 | `bridge_runtime_goroutines`、`bridge_runtime_heap_alloc_bytes`、`bridge_runtime_sys_bytes`、`bridge_runtime_gc_total` | Gauge；goroutine、堆、系统内存和 GC 次数 |
| 配额 | `bridge_connection_limit`、`bridge_connection_limit_per_port` | Gauge；0 表示不限制 |

单桥指标使用 `proxy_addr` 标签是为了直接定位目标代理，但代理切换会产生新的时间序列；
如果代理地址频繁变化，应在 Prometheus 中设置合理的保留期。不会把错误文本、客户端 IP
或每条连接 ID 放入 label，避免高基数和额外内存开销。

当前没有直接实现 Rust bridge 中依赖操作系统网卡接口的
`bridge_machine_bandwidth_*` / `bridge_machine_network_bytes_*`，也没有实现按客户端 IP
聚合的 `bridge_max_proxy_ips`：前者需要分别适配 Linux `/proc`、Windows 性能计数器等数据源，
后者会引入客户端 IP 高基数和隐私风险。可以后续通过独立的 node-exporter/Windows exporter
补充机器网卡指标；单桥流量已经由 `bridge_*_relay_bytes_total` 提供。

性能影响：指标端点关闭时，只有连接接受、拒绝、拨号和转发完成路径上的原子计数更新；
不会执行网络探测、写日志或分配 Prometheus label。抓取 `/metrics` 时才遍历 listener、读取
运行时内存并生成文本；不在数据转发锁内执行，因此正常抓取不会阻塞 add/del 或流量转发。
`bridge_dial_duration_seconds` 使用固定 10 桶直方图，采集成本有上界。若不需要延迟直方图，
可在 Prometheus 抓取配置中丢弃该指标。

Prometheus 的运行时 gauge 与原有 `startStatsLogger`、`/bridge/status` 的 `stats` 共用同一套
运行时快照采集逻辑：桥数量、监听数量、活动连接、accepted/rejected、dial 成功/失败、goroutine、堆、
系统内存和 GC 不再分别遍历和读取。需要注意，Prometheus 的 `*_total` 事件指标是进程生命周期
累计值；旧 `stats` 中的 accepted/rejected/dial 数值仍表示当前运行 listener 的水位，删除 bridge
后不会把已删除 listener 的计数带入旧日志。

常用查询示例：

```promql
sum(bridge_connections_active_total)
rate(bridge_relay_bytes_total[5m])
rate(bridge_dials_total{result="failure"}[5m])
bridge_listeners - sum(bridge_listener_up)
histogram_quantile(0.95, sum by (le) (rate(bridge_dial_duration_seconds_bucket[5m])))
```

## 代理列表格式

批量工具统一支持以下代理格式：

- TXT：每行一个代理 URL，例如 `socks5://user:password@198.51.100.10:1080`；首行是 `proxy` 时自动跳过
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

### 代码级真实进程 e2e

项目中的 `e2e/` 测试会启动真实 bridge 进程、真实本地 SOCKS5 测试服务，并通过
`myip.ipipv.com` 验证转发、同端口切换、不可达目标恢复、并发 churn 和资源水位：

```powershell
go test -tags e2e ./e2e -v
```

只运行基础转发和同端口切换：

```powershell
go test -tags e2e ./e2e -run 'TestE2ESocks5ViaBridge|TestE2ESameBridgePortSwitchesProxy' -v
```

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

`auth_failed` 只有在 SOCKS5 服务器明确选择用户名/密码认证（方法 `0x02`），并返回非零认证
状态码时才会产生。输出中的 `status=0xNN` 和 `raw=01 NN` 是 SOCKS5 子协商的原始字节；协议
本身不提供文字错误信息，因此 `general SOCKS server failure` 等文字是脚本对状态码的解释，
不是代理返回的原始文本。如果代理在认证阶段直接断开，则会归类为 `proxy_closed`；如果代理
选择 `0x00` 无认证，即使 URL 中提供了用户名密码，也只能说明本次连接未验证这些凭据。

查看任一工具的完整参数说明：

```powershell
go run ./cmd/proxy-db -h
go run ./cmd/proxy-e2e -h
python .\scripts\convert_bridge11.py --help
python .\scripts\test_socks5_proxies.py --help
```
