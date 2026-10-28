# Hysteria 2 官方内核

VLESS+Reality/Trojan 使用 Xray，hy2 使用独立官方 Hysteria 2。当前固定 v2.13.0，支持 Direct、URI 和 Mihomo；hy2 One-Hop 与 sing-box 导出暂不可用。128MB 双内核和公网 NAT 结果必须单独验收，不能继承单 Xray 的资源结论。

## 安装与运行

安装器接受 `--hysteria-bin /绝对路径/hysteria`，会按当前架构核对官方二进制 SHA256，安装到 `/var/lib/panel-agent/agent/runtime/hysteria` 并记录所有权。与 `--xray-bin` 一起传入可启用两套内核；只传 Hysteria 时使用 hy2-only 模式。项目二进制清单不接管用户原有 Hysteria 服务。

```sh
sudo ./panel-agent-installer install --role agent \
  --agent-bin /绝对路径/control-agent --hysteria-bin /绝对路径/hysteria
```

服务默认仅 enable，完成配对后再启动。手动运行已配对 Agent：

```sh
control-agent run --data-dir /var/lib/panel-agent/agent \
  --hysteria-path /var/lib/panel-agent/agent/runtime/hysteria
```

需要 Reality 时同时传 `--xray-path`。`sync` 是一次性操作，hy2 认证需要常驻 Agent，应以 `run` 或安装器生成的 systemd 服务提供持续服务。

官方 v2.13.0 资产校验值：

| 架构 | SHA256 |
|---|---|
| amd64 | `907ba8c9693edb104b20582681fb7dc15639d5b64a9cbb616a7b539190a86691` |
| arm64 | `a68a61a84452ca250ce0368202521965ca9cc9d801a404f1dc9008ac6cf677a7` |

## 创建节点

把 `examples/extensions/official-hysteria2/package.json` 压缩为只含该文件的 ZIP，在系统设置的协议扩展页导入并启用。创建节点时选中该扩展，填写 TLS SNI、匹配的证书和私钥；Reality 专用字段不参与此扩展的协议渲染。选择本机直出，按原有用户组和节点组授权。

懒猫 NAT 需要先配置公网 UDP 端口到本机监听 UDP 端口。节点公网端口填提供商的外部映射端口，监听端口填机器内的目标端口；两个端口可以不同。域名使用 DNS only，证书覆盖 TLS SNI；客户端默认校验证书，不通过关闭验证掩盖无效证书。普通 TCP 探测不适用于 hy2，需用真实客户端验证 QUIC、TCP/UDP 业务及入账。

## 生命周期与恢复

每个 hy2 入口为独立受管实例，权限、到期和三维配额来自 Agent 本地快照。撤销会停止受影响实例以终止已有连接；随后只为仍获授权的用户重建。Panel 暂时不可用时保持本地权限、继续采样，恢复后补报。Agent 退出会终止 hy2；再次启动时先确认旧实例停止，再重放旧来源并建立新计数来源。无法观测的退出尾部标记 gap，不宣称完整无损。

普通卸载保留配置、证书和未 ACK 账本；只有显式 purge 删除项目自己的数据。当前版本仅接受固定官方资产，手动替换其他版本会被拒绝。独立版本升级/失败回滚的运维闭环尚需补齐，不应直接替换正在运行的二进制。

官方依据：[服务端配置](https://v2.hysteria.network/docs/advanced/Full-Server-Config/)、[统计 API](https://v2.hysteria.network/docs/advanced/Traffic-Stats-API/)、[URI](https://v2.hysteria.network/docs/developers/URI-Scheme/)。
