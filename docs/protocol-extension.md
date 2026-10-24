# 声明式协议扩展格式

Panel-Agent v1 的扩展包由外部工具生成，Panel 只接收并静态验证 ZIP，不调用模型 API，也不执行包内代码。ZIP 必须只包含一个 `package.json`；压缩内容不超过 1 MiB，展开内容不超过 4 MiB，条目不超过 32 个，路径深度不超过 4，绝对路径、`..`、符号链接、硬链接和特殊文件都会拒绝。未知 JSON 字段、脚本、动态库、Shell、模板函数、任意表达式、网络地址和任意文件路径都不属于格式。

`package.json` 的顶层必须是 `manifest`、`schema`、`renderers` 三个对象。V1 当前只接受固定的 `xray.trojan_tls`、`uri.trojan_tls` 和 `mihomo.trojan_tls` renderer；它们会被 Panel 编译为固定的 Xray Trojan/TCP/TLS 入站和对应客户端输出，不能通过包覆盖 API、Stats、日志、路由或端口边界。`one_hop`、`sing_box` 以及未实现的能力必须声明为 `false`。

最小示例的字段固定为 `sni`、`certificate_pem`、`private_key_pem`。SNI 是域名纯文本；证书和私钥必须是匹配的 PEM，Panel 解析证书、解析私钥并校验公钥匹配后才接受。SNI 只保存为可展示元数据，证书与私钥使用 Panel master key 加密保存，普通 Node/API/用户订阅不会返回私钥；下发给 Agent 的已确认快照仍由既有 machine snapshot 加密存储和权限边界保护。

示例包位于 [`examples/extensions/trojan-tls-v1/package.json`](../examples/extensions/trojan-tls-v1/package.json)。在 PowerShell 中制作导入包：

```powershell
Compress-Archive -Path .\examples\extensions\trojan-tls-v1\package.json -DestinationPath .\trojan-tls-v1.zip -Force
```

登录管理员后打开“协议扩展”导入 ZIP。导入版本默认停用；启用后，节点编辑页才允许新建该扩展引用。节点引用包含明确的扩展 ID 和版本，保存时重新验证动态字段；停用扩展会保留 Node 定义但让它从已生成 Core 配置和正式订阅中消失，重新启用后恢复。删除被 Node 引用的版本会拒绝，必须先把节点切回内置 Reality 或删除节点引用。

生命周期固定为：导入 → 静态验证 → 在隔离机器执行 Xray `-test` → 启用 → 节点显式引用 → 停用/解除引用 → 删除。`-test` 只验证最终编译出的 Xray 结构，不能替代真实公网、证书链、客户端握手或出站验收；当前示例不支持 One-Hop、UDP 和 sing-box，页面与订阅不会生成这些假输出。
