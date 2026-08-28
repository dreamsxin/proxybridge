# DBridge - 动态桥代理服务

DBridge 是一个高性能的代理桥接服务，支持 HTTP 和 SOCKS5 协议。它作为客户端与上游代理服务器之间的桥梁，提供认证、带宽限制和流量统计等功能。

## 功能特性

- **双协议支持**：同时支持 HTTP 和 SOCKS5 入站代理，自动识别协议类型
- **上游代理**：支持通过 HTTP 和 SOCKS5 代理服务器访问目标
- **智能认证**：多层认证机制，支持凭证缓存和失败重试限制
- **IP 保护**：内置 IP 封禁机制，防止暴力破解
- **带宽控制**：基于令牌桶的带宽限制器，精确控制流量
- **连接池**：高效的连接池管理，支持动态调整
- **状态上报**：定期向中心服务器上报运行状态
- **配置管理**：从中心服务器动态获取配置

## 项目结构

```
dbridge/
├── internal/
│   ├── auth/           # 认证模块
│   │   ├── authenticator.go  # 认证器核心逻辑
│   │   ├── cache.go          # 成功/失败缓存
│   │   └── guard.go          # IP 封禁保护
│   ├── center/        # 中心服务器通信
│   │   ├── client.go   # API 客户端
│   │   ├── config.go   # 配置管理
│   │   └── reporter.go # 状态上报
│   ├── forward/       # 数据转发
│   │   └── relay.go    # 双向流量转发
│   ├── proxy/         # 入站代理服务
│   │   ├── server.go   # 协议分发器
│   │   ├── http.go     # HTTP 代理处理
│   │   └── socks5.go   # SOCKS5 代理处理
│   └── upstream/      # 上游代理管理
│       ├── dialer.go   # 代理连接
│       ├── limiter.go  # 带宽限制
│       └── pool.go     # 连接池管理
├── pkg/
│   └── crypto/        # 加密工具
│       └── aes.go     # AES 加解密
├── config.toml       # 配置文件
├── go.mod            # Go 模块定义
└── bridge_client      # 编译后的可执行文件
```

## 快速开始

### 安装

```bash
# 克隆项目
git clone <repository-url>
cd dbridge

# 下载依赖
go mod download

# 编译
go build -o bridge_client .
```

### 配置

编辑 `config.toml` 文件：

```toml
# 入站代理监听地址（HTTP/SOCKS5 共用端口）
proxy_addr = ":7890"

[log]
# 日志级别：debug、info、warn、error
level = "info"

# 日志文件路径；留空或删除该字段时输出到标准输出
file = "logs/dbridge.log"

# 单个日志文件最大大小，单位 MB；达到后自动切割
max_size_mb = 100

[center]
# 中心服务器地址
url = "http://127.0.0.1:8080"

# 桥 ID（在中心服务器 bridge_gateway 表中的 id）
bridge_id = 1

# 桥鉴权密钥（对应 bridge_gateway.secret 字段）
bridge_secret = "your-bridge-secret"
```

### 运行

```bash
./bridge_client
```

## 使用方法

### 作为 HTTP 代理

```bash
# 使用 curl 测试
curl -x http://username:password@localhost:7890 http://httpbin.org/ip

# 在浏览器中配置
代理类型：HTTP
服务器：localhost
端口：7890
用户名/密码：你的凭证
```

### 作为 SOCKS5 代理

```bash
# 使用 curl 测试
curl -x socks5://username:password@localhost:7890 http://httpbin.org/ip

# 或者使用 proxychains
proxychains curl http://httpbin.org/ip
```

## 认证流程

1. **协议识别**：根据连接首字节判断是 HTTP (0x05) 还是 SOCKS5
2. **凭证解析**：从请求中提取用户名和密码
3. **IP 检查**：检查客户端 IP 是否被封禁
4. **缓存查询**：先检查本地缓存（成功/失败缓存）
5. **中心验证**：向中心服务器发送认证请求
6. **结果处理**：
   - 成功：获取上游代理信息并建立连接
   - 失败：记录失败次数，达到阈值时封禁 IP

## 带宽控制

- 使用令牌桶算法实现带宽限制
- 每个上游代理独立限速
- 支持通过认证响应中的 `speed_limit` 为单个用户设置不同限制
- 默认限制：5 MB/s

## IP 封禁规则

系统采用滑动窗口封禁策略：

| 时间窗口 | 失败次数阈值 | 封禁时长 |
|---------|-------------|---------|
| 1 分钟  | 5 次        | 1 分钟  |
| 3 分钟  | 10 次       | 5 分钟  |
| -       | -           | 最大 10 分钟 |

## API 接口

### 中心服务器接口

认证接口（POST `/api/open/bridge/auth`）：
```json
{
  "bridge_id": 1,
  "bridge_secret": "secret",
  "username": "user",
  "password": "pass",
  "client_ip": "192.168.1.1"
}
```

配置接口（POST `/api/open/bridge/config`）：
```json
{
  "bridge_id": 1,
  "bridge_secret": "secret"
}
```

响应示例：
```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "protocol": "socks5",
    "ip": "1.2.3.4",
    "port": 1080,
    "proxy_user": "upstream_user",
    "proxy_pass": "upstream_pass",
    "speed_limit": 5242880
  }
}
```

## 性能优化

- **连接池复用**：上游代理连接池减少建立开销
- **缓冲区复用**：32KB 缓冲区池，峰值内存 = 池大小 × 32KB
- **零拷贝**：使用 `io.CopyBuffer` 减少内存拷贝
- **异步处理**：每个连接独立 goroutine 处理

## 依赖

- Go 1.26+
- github.com/BurntSushi/toml - TOML 配置解析
- golang.org/x/net - 网络工具
- golang.org/x/time - 限速器

## 注意事项

1. 确保中心服务器可达，否则认证服务不可用
2. 生产环境请使用 HTTPS 中心服务器地址
3. 建议配合防火墙限制访问监听端口
4. 监控日志中的警告信息，及时处理异常情况

## 日志

程序使用结构化日志（slog），主要日志级别：
- `INFO`：服务启动、连接建立
- `WARN`：认证失败、连接错误
- `DEBUG`：SOCKS5 协议交互详情

日志可通过 `config.toml` 的 `[log]` 节配置：

```toml
[log]
level = "info"
file = "logs/dbridge.log"
max_size_mb = 100
```

- `level` 支持 `debug`、`info`、`warn`、`error`，默认 `info`
- `file` 为空或不配置时输出到标准输出；配置后写入指定文件，目录会自动创建
- `max_size_mb` 为单个日志文件大小上限，默认 `100`；超过后会将旧文件重命名为带时间戳的文件，并继续写新日志
