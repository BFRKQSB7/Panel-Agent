# Trojan TLS v1 受限扩展示例

这是 Panel-Agent 的声明式扩展示例。导入时把本目录中的 `package.json` 压缩为只包含该文件的 ZIP；Panel 只接受固定字段、固定 Trojan/TCP/TLS Xray renderer 和 URI/Mihomo renderer，不执行包内代码，也不会读取包外路径。

节点引用扩展后，SNI、证书 PEM 和私钥 PEM 由管理员在节点页提交。SNI 明文只作为订阅/配置元数据保存，证书与私钥使用 Panel master key 加密保存；扩展停用会保留 Node 定义但阻止该 Node 进入 Core 和用户订阅。该示例不支持 One-Hop、UDP 或 sing-box，页面不会伪造这些输出。
