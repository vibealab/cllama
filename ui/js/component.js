// cllama dashboard UI components.
//
// Small dependency-free building blocks shared by the app code:
//   UI.el / UI.badge  – DOM helpers
//   UI.modal          – modal dialog on a dimmed mask (stackable)
//   UI.confirm        – promise-based confirm dialog built on modal
//   UI.toast          – transient bottom-right notification
//
// Exposes a single global: UI.
window.UI = (() => {
  "use strict";

  // ── DOM helpers ─────────────────────────────────────────────────────────
  function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  }

  function badge(text, cls) {
    return el("span", "badge " + (cls || ""), text);
  }

  // ── mask + modal stack ──────────────────────────────────────────────────
  // A single dimmed mask sits under all dialogs; Escape or a mask click
  // closes only the topmost dialog, so confirm() can nest above modal().
  const mask = el("div", "mask hidden");
  const stack = [];

  function syncMask() {
    mask.classList.toggle("hidden", stack.length === 0);
  }

  /**
   * UI.modal({ title, content, actions, onClose })
   *   content  – Node rendered in the dialog body
   *   actions  – [{ label, className, onClick(close, button) }]
   *   onClose  – called whenever the dialog closes (also via mask/Escape)
   * Returns { close() }.
   */
  function modal(opts) {
    opts = opts || {};
    const root = el("div", "modal");

    const head = el("div", "modal-head");
    head.appendChild(el("h3", null, opts.title || ""));
    const closeBtn = el("button", "modal-close", "✕");
    closeBtn.type = "button";
    closeBtn.title = "Close";
    head.appendChild(closeBtn);
    root.appendChild(head);

    const body = el("div", "modal-body");
    if (opts.content) body.appendChild(opts.content);
    root.appendChild(body);

    const dlg = {};

    function close() {
      const i = stack.indexOf(dlg);
      if (i === -1) return; // already closed
      stack.splice(i, 1);
      root.remove();
      if (opts.onClose) opts.onClose();
      syncMask();
    }
    dlg.close = close;
    closeBtn.addEventListener("click", close);

    const actions = opts.actions || [];
    if (actions.length) {
      const foot = el("div", "modal-foot");
      for (const a of actions) {
        const btn = el("button", a.className || "", a.label);
        btn.type = "button";
        btn.addEventListener("click", () => a.onClick(close, btn));
        foot.appendChild(btn);
      }
      root.appendChild(foot);
    }

    document.body.appendChild(root);
    stack.push(dlg);
    syncMask();
    return dlg;
  }

  mask.addEventListener("click", () => {
    const top = stack[stack.length - 1];
    if (top) top.close();
  });
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") {
      const top = stack[stack.length - 1];
      if (top) top.close();
    }
  });

  /**
   * UI.confirm({ title, message, okLabel, cancelLabel, danger })
   * Resolves true when confirmed, false when dismissed any other way.
   */
  function confirm(opts) {
    opts = opts || {};
    return new Promise((resolve) => {
      let settled = false;
      const settle = (v, close) => {
        if (settled) return;
        settled = true;
        resolve(v);
        if (close) close();
      };
      modal({
        title: opts.title || "Confirm",
        content: el("div", "confirm-msg", opts.message || ""),
        actions: [
          {
            label: opts.cancelLabel || "Cancel",
            onClick: (close) => settle(false, close),
          },
          {
            label: opts.okLabel || "OK",
            className: opts.danger ? "danger-solid" : "primary",
            onClick: (close) => settle(true, close),
          },
        ],
        onClose: () => settle(false),
      });
    });
  }

  // ── toast ───────────────────────────────────────────────────────────────
  const toastBox = el("div", "toast-box");

  function toast(message, type) {
    const t = el("div", "toast " + (type === "bad" ? "toast-bad" : "toast-ok"), message);
    toastBox.appendChild(t);
    setTimeout(() => {
      t.classList.add("fade");
      setTimeout(() => t.remove(), 400);
    }, type === "bad" ? 6000 : 3000);
  }

  // Mount the shared chrome (scripts load at the end of <body>).
  document.body.appendChild(mask);
  document.body.appendChild(toastBox);

  return { el, badge, modal, confirm, toast };
})();
