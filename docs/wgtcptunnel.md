# WireGuard TCP 封装工具

该程序提供了一个独立的 Go 实现，用于将 WireGuard 报文通过 TCP 连接进行转发。在网络不允许直接使用 UDP 的环境中，可以配合客户端与服务端两个模式，将 WireGuard 的 UDP 流量封装到单一的 TCP 连接中。

## 特性

- **双模式**：
  - `client` 模式监听本地 UDP 端口（例如 WireGuard 客户端配置中的 `Endpoint`），并将报文通过 TCP 转发到服务器。
  - `server` 模式监听 TCP 端口，收到的数据会转发到指定的 WireGuard UDP 端点，同时把返回的 UDP 数据重新封装回 TCP。
- **长度前缀封装**：TCP 流中的每个 WireGuard 报文都使用 4 字节大端长度前缀，确保报文边界准确恢复。
- **自动重连**：客户端在连接断开时会按照设定的时间间隔自动重连。
- **上下文取消**：通过系统信号（`SIGINT`, `SIGTERM`）可以优雅停止程序。

## 编译

```bash
go build ./cmd/wgtcptunnel
```

## 使用示例

### 客户端

```bash
wgtcptunnel \
  --mode client \
  --udp-listen 127.0.0.1:51820 \
  --tcp 198.51.100.10:443 \
  --reconnect 5s
```

将 WireGuard 客户端的 `Endpoint` 指向 `127.0.0.1:51820`，程序会负责和远端服务器建立 TCP 连接。

### 服务端

```bash
wgtcptunnel \
  --mode server \
  --udp-listen :51820 \
  --udp-target 10.0.0.1:51820 \
  --tcp :443
```

服务端会在本地监听 TCP `:443`，收到的报文转发到 WireGuard 服务器 `10.0.0.1:51820`。

> 提示：根据部署环境调整 `--udp-listen`（作为 WireGuard 对端的源端口）与 `--udp-target`（实际 WireGuard 服务器地址）。

## 封装格式

每个 WireGuard 报文在发送前都会添加 4 字节大端编码的长度。接收方在读取长度后再读取对应数量的字节，从而保证 TCP 流中能准确区分出每一帧报文。

## 运行时日志

程序会输出连接建立、重连、丢弃异常数据等日志，方便排查问题。
