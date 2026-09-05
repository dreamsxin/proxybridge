# 从代理列表生成 bridge.db

命令 `cmd/proxy-db` 将完整代理 URL 列表转换为 bridge.db：

```text
代理列表: socks5://user:pass@198.51.100.10:1080
bridge.db: 10000,198.51.100.10:1080
bridge.csv: 10000,socks5,198.51.100.10:1080,user,password
```

用户名和密码不会写入 bridge.db。bridge-direct 只需要通过 TCP 连接到代理地址；代理认证由后续客户端的 SOCKS5/HTTP 协议握手完成。配套的 bridge.csv 会保留认证信息，供批量测试或审计使用。

## 代理文件

TXT 每行一个代理，或 CSV 第一列为代理地址。支持 `socks5://`、`socks5h://`、`http://` 和不带 scheme 的 `host:port`。代理主机可以是 IPv4、IPv6 或域名，生成时保留主机名，不执行 DNS 解析。

## 运行

建议先停止正在使用该 bridge.db 的 bridge-direct 进程，再执行：

```powershell
go run ./cmd/proxy-db `
  -proxy-file D:\work\data-1788313833471.csv `
  -output D:\work\server-bridge-direct\bridge.db `
  -bridge-port-start 10000
```

默认同时生成 `D:\work\server-bridge-direct\bridge.csv`。也可以指定 CSV 路径：

```powershell
go run ./cmd/proxy-db `
  -proxy-file D:\work\data-1788313833471.csv `
  -output D:\work\server-bridge-direct\bridge.db `
  -csv-output D:\work\server-bridge-direct\proxy-bridge.csv
```

参数：

| 参数 | 缺省 | 说明 |
|---|---:|---|
| `-proxy-file` | 必填 | TXT 或 CSV 代理列表 |
| `-output` | `bridge.db` | 输出文件路径；会整体替换已有内容 |
| `-csv-output` | 根据 `-output` 推导 | 保留代理 scheme、地址、用户名和密码的配套 CSV 路径 |
| `-bridge-port-start` | `10000` | 第一个代理分配的 bridgePort，之后连续递增 |
| `-dedupe` | false | 按代理 host:port 去重，默认按每一行生成 |
| `-verbose` | false | 打印每条生成记录 |

示例输出：

```text
generated 94 bridge entries from D:\work\data-1788313833471.csv
output=D:\work\server-bridge-direct\bridge.db bridgePorts=10000-10093
credentials CSV=D:\work\server-bridge-direct\bridge.csv
```

CSV 格式：

```csv
bridgePort,proxyScheme,proxyAddr,username,password
10000,socks5,198.51.100.10:1080,user,password
```

输出使用现有 `CacheF.Replace` 原子写入；空列表、非法端口范围和输出目录不存在都会直接失败，不会生成部分 bridge.db。
