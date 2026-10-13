# Gateway 抓包工具

此目录可以整体复制到其他 Mac / Linux 机器。客户端只需要 Python 3.9+ 和 OpenSSH，不依赖 Ember 服务、数据库或第三方 Python 包。

```bash
python3 -B gateway_capture.py --help
python3 -B -m unittest discover -s tests -v
```

- `capture`：通过显式 SSH 路径，进入已有 Docker Gateway 的网络命名空间，限时、限容量采集 IPv4 TCP。
- `logs`：在远端过滤指定时间段的 Gateway 日志，再写入本机 JSON。
- `analyze`：离线解析本机 PCAP，输出 HTTP、TLS 握手和视频请求响应耗时。
- `capture --dry-run`：只打印配置，不连接 SSH、不创建文件。

跨机器配置、完整命令、权限要求、证据边界和历史样本见 [Gateway 抓包与播放排查](../../docs/runbooks/gateway-packet-capture.md)。若独立复制本目录，请同时带走该手册。

采集和分析产物必须放在仓库外的私有目录。原始 PCAP 可能包含鉴权信息，不随工具分发。工具不会上传文件、安装软件、启动或重启 Ember 服务。
