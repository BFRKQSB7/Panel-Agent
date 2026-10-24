"use strict";
(() => {
  const csrf = document.querySelector('input[name="csrf"]')?.value || "";
  const copy = async (button) => {
    const target = document.getElementById(button.dataset.copyTarget);
    const status = button.parentElement?.querySelector("[data-copy-status]");
    if (!target) return;
    try {
      await navigator.clipboard.writeText(target.value || target.textContent || "");
      if (status) status.textContent = "已复制";
    } catch {
      target.focus(); target.select();
      if (status) status.textContent = "请按 Ctrl+C 复制";
    }
  };
  const check = async (button) => {
    const status = button.parentElement?.querySelector("[data-check-status]");
    button.disabled = true;
    if (status) status.textContent = "检查中…";
    try {
      const response = await fetch(button.dataset.checkPath, {method: "POST", headers: {"Content-Type": "application/json", "X-CSRF-Token": csrf}, body: JSON.stringify({mode: button.dataset.checkMode || "public"})});
      const result = await response.json();
      if (status) status.textContent = `${result.dns?.status || "unknown"} / ${result.tcp?.status || "unknown"}：${result.tcp?.detail || result.dns?.detail || result.error || "无结果"}`;
    } catch {
      if (status) status.textContent = "检查请求失败；请查看管理 API 响应。";
    } finally {
      button.disabled = false;
    }
  };
  document.addEventListener("click", (event) => {
    const copyButton = event.target.closest("[data-copy-target]");
    if (copyButton) { event.preventDefault(); void copy(copyButton); return; }
    const checkButton = event.target.closest("[data-check-path]");
    if (checkButton) { event.preventDefault(); void check(checkButton); }
  });
})();
