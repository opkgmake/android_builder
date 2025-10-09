# network_bsd.cpp 代码概览

该文件实现了 TunSafe 项目在类 BSD/Linux 平台上的底层网络循环：

* **`NetworkBsd`**：事件循环核心，使用 `poll`/`ppoll` 监听多个套接字，处理轮询、定时任务以及主线程调度。
* **`TunSocketBsd`**：封装 TUN 设备的读写逻辑，负责从虚拟网卡读取数据并交给 WireGuard 协议栈处理，同时将待发送的数据写回 TUN 设备。Linux/Android 上没有 4 字节前缀，macOS/FreeBSD 则需要。
* **`UdpSocketBsd`**：处理 UDP 监听端口。这里将地址族固定为 `AF_INET6`，使用 `sockaddr_in6` 结构，从而既能收发 IPv6 也能兼容 IPv4 映射地址。
* **`UnixDomainSocketListenerBsd` / `UnixDomainSocketChannelBsd`**：监听 `/var/run/wireguard/*.sock` 管理接口，接收来自 `wg` 工具的配置更新。
* **`TcpSocketBsd`**：实现将 WireGuard 报文封装在 TCP 连接上的逻辑，包括握手失败时的重连、报文队列、`writev`/`readv` 批量读写等。
* **辅助结构**：例如 `NotificationPipeBsd` 用管道唤醒主循环、`UnixSocketDeletionWatcher` 监控 Unix 套接字删除事件。

整体代码属于 [TunSafe](https://tunsafe.com) 的 WireGuard 用户态实现，负责跨平台的套接字抽象与事件调度。
