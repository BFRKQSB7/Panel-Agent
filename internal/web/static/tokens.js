"use strict";
(() => {
  const prefix = "panel-agent-token:";
  const syntax = /^[A-Za-z0-9_-]{43,128}$/;
  const now = Date.now();
  const key = (id, kind) => `${prefix}${id}:${kind}`;
  const read = (id, kind, generation) => {
    try {
      const storageKey = key(id, kind);
      const item = JSON.parse(sessionStorage.getItem(storageKey) || "null");
      if (item && item.expires > Date.now() && item.generation === generation && syntax.test(item.token)) return item.token;
      sessionStorage.removeItem(storageKey);
    } catch { /* Storage may be disabled; one-time delivery remains visible. */ }
    return "";
  };
  const show = (container, kind, token) => {
    const formats = kind === "dashboard" ? [["用户页链接", `/u/${token}`]] : [["URI 节点列表", `/s/${token}/uri`], ["Mihomo 配置", `/s/${token}/mihomo`]];
    for (const [label, path] of formats) {
      const url = new URL(path, location.origin).href;
      const row = document.createElement("label");
      row.append(document.createTextNode(label + " "));
      const input = document.createElement("input");
      input.readOnly = true; input.value = url; input.autocomplete = "off"; input.spellcheck = false;
      row.append(input);
      const link = document.createElement("a");
      link.href = url; link.textContent = "打开 / 下载"; link.rel = "noreferrer";
      row.append(link); container.append(row);
    }
  };
  // Keep the original deadline on reload: reading never renews the 30-minute TTL.
  for (const item of document.querySelectorAll("[data-token-delivery]")) {
    const {userId, kind, token, generation} = item.dataset;
    if ((kind !== "dashboard" && kind !== "subscription") || !syntax.test(token)) continue;
    try { sessionStorage.setItem(key(userId, kind), JSON.stringify({token, generation, expires: now + 30 * 60 * 1000})); } catch {}
    show(item.querySelector(".delivery-links"), kind, token);
  }
  for (const row of document.querySelectorAll("[data-token-state]")) {
    const container = row.querySelector(".cached-links");
    for (const kind of row.hasAttribute("data-dashboard-page") ? ["subscription"] : ["dashboard", "subscription"]) {
      const token = read(row.dataset.userId, kind, row.dataset[`${kind}Generation`]);
      if (token) show(container, kind, token);
    }
    if (!container.childElementCount) container.textContent = "当前标签页没有可用的临时链接，服务端无法恢复旧 Token。";
  }
  document.getElementById("forget-tokens")?.addEventListener("click", () => {
    try {
      for (const name of Object.keys(sessionStorage)) if (name.startsWith(prefix)) sessionStorage.removeItem(name);
    } catch {}
    for (const links of document.querySelectorAll(".cached-links,.delivery-links")) links.replaceChildren();
    // Remove plaintext from the current DOM as well as storage. The server has
    // already discarded it; a new GET cannot deliver it again.
    for (const delivery of document.querySelectorAll("[data-token-delivery]")) {
      delete delivery.dataset.token;
      for (const input of delivery.querySelectorAll("input")) input.value = "";
    }
    document.getElementById("token-cache-message").textContent = "已清除临时 Token；已交付或已导入的凭证仍然有效。";
  });
})();
