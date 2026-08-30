// cllama web console — settings tab.
//
// Renders the server's in-memory system configuration (GET /admin/config),
// one row-div per setting; every setting is editable inline (✏️) and saved
// to the server with a partial PUT /admin/config, applying immediately.
// app.js drives refresh() on "hello" and "config" SSE topics; the tab also
// has a manual refresh.
//
// Auth tokens display masked (••••): 👁 reveals the plain text (fetched
// lazily from /admin/config/secret/<name>), 🙈 masks it again; editing a
// token accepts an empty value to disable the respective auth.
// Exposes a single global: Settings.
window.Settings = (() => {
  "use strict";

  const el = UI.el;
  const badge = UI.badge;

  // ── value formatters ────────────────────────────────────────────────────
  // Retentions are whole seconds on the wire:
  // -1 = stay in the queue forever (manual removal only),
  //  0 = leave the queue immediately,
  // >0 = stay in the queue that long, then get purged.
  function fmtRetention(sec) {
    if (sec === -1) return badge("forever (manual removal)", "warn");
    if (sec === 0) return badge("0 s (no stay in queue)", "muted");
    return badge(sec + " s", "ok");
  }

  // Generation timeout: 0 = no timeout, >0 = abort upstream after N seconds.
  function fmtTimeout(sec) {
    if (sec <= 0) return badge("no timeout", "muted");
    return badge(sec + " s", "ok");
  }

  // ── setting definitions ─────────────────────────────────────────────────
  const SETTINGS = [
    {
      kind: "int",
      key: "max_queue",
      label: "Max queue size",
      min: 0,
      desc: "Requests held while no backend serves their model (0 disables queueing).",
      fmt: (v) => (v === 0 ? badge("queueing off", "muted") : String(v)),
    },
    {
      kind: "int",
      key: "done_retention_sec",
      label: "Done retention",
      min: -1,
      desc: "How long completed requests stay in the queue: -1 forever (manual removal), 0 no stay (default), >0 seconds to keep.",
      fmt: fmtRetention,
    },
    {
      kind: "int",
      key: "failed_retention_sec",
      label: "Failed retention",
      min: -1,
      desc: "Same semantics for failed requests.",
      fmt: fmtRetention,
    },
    {
      kind: "int",
      key: "gen_timeout_sec",
      label: "Generation timeout",
      min: 0,
      desc: "Abort an upstream generation request after this many seconds (0 = no timeout). Applies to clients of backends registered after a change.",
      fmt: fmtTimeout,
    },
    {
      kind: "secret",
      key: "api_token",
      flag: "has_api_token",
      label: "API token",
      placeholder: "new token (empty disables auth)",
      desc: "Bearer token required on LLM API and model-list requests (seeds from -token). Empty disables auth; changes apply immediately.",
    },
    {
      kind: "secret",
      key: "parent_auth",
      flag: "has_parent_auth",
      label: "Parent auth token",
      placeholder: "new token (empty disables tunnel auth)",
      desc: "Token child cllama servers must present to tunnel into this server (seeds from -parentauth). Empty disables tunnel auth.",
    },
  ];

  // Icon action button (emoji glyph, meaning in the tooltip).
  function iconButton(glyph, title, handler) {
    const b = el("button", "icon-btn", glyph);
    b.type = "button";
    b.title = title;
    b.addEventListener("click", handler);
    return b;
  }

  // ── one row-div per setting, inline editing ─────────────────────────────
  function renderSetting(cfg, def) {
    const root = el("div", "setting");

    const nameCell = el("div", "setting-name");
    nameCell.textContent = def.label;

    const body = el("div", "setting-body");
    const valueRow = el("div", "setting-value mono");
    const desc = el("p", "setting-desc");
    desc.textContent = def.desc;
    body.append(valueRow, desc);
    root.append(nameCell, body);

    let revealed = null; // cached plain token text while 👁 is active; null = masked

    async function toggleReveal() {
      if (revealed !== null) {
        revealed = null; // mask again
        paintView();
        return;
      }
      try {
        const data = await API.configSecret(def.key);
        revealed = data.value || "";
      } catch (err) {
        UI.toast("reveal failed: " + err.message, "bad");
        return;
      }
      paintView();
    }

    function paintView() {
      valueRow.replaceChildren();
      valueRow.appendChild(iconButton("✏️", "Edit", startEdit));
      if (def.kind === "secret") {
        if (!cfg[def.flag]) {
          valueRow.appendChild(badge("no token (auth off)", "muted"));
        } else {
          valueRow.appendChild(
            iconButton(revealed === null ? "👁️" : "🙈", revealed === null ? "Reveal" : "Hide", toggleReveal)
          );
          const display = el("span");
          display.textContent = revealed === null ? "••••••••" : revealed || "(empty)";
          valueRow.appendChild(display);
        }
      } else {
        const rendered = def.fmt(cfg[def.key]);
        if (rendered instanceof Node) valueRow.appendChild(rendered);
        else valueRow.appendChild(document.createTextNode(String(rendered)));
      }
    }

    function startEdit() {
      const form = el("div", "setting-edit");
      const input = el("input");
      if (def.kind === "int") {
        input.type = "number";
        input.min = String(def.min);
        input.step = "1";
        input.value = String(cfg[def.key]);
      } else {
        input.type = "text";
        input.autocomplete = "new-password";
        input.spellcheck = false;
        input.placeholder = def.placeholder;
      }
      const saveBtn = el("button", "primary", "💾 Save");
      saveBtn.type = "button";
      const cancelBtn = el("button", null, "Cancel");
      cancelBtn.type = "button";
      form.append(saveBtn, cancelBtn, input);
      valueRow.replaceChildren(form);
      input.focus();
      input.select();

      async function doSave() {
        let value;
        if (def.kind === "int") {
          value = Number.parseInt(input.value, 10);
          if (!Number.isInteger(value) || value < def.min) {
            UI.toast(def.label + ": enter a whole number ≥ " + def.min, "bad");
            return;
          }
        } else {
          value = input.value;
        }
        saveBtn.disabled = true;
        try {
          // Partial update: the server keeps every other setting.
          await API.updateConfig({ [def.key]: value });
          UI.toast(def.label + " saved");
          refresh();
        } catch (err) {
          saveBtn.disabled = false;
          UI.toast("save failed: " + err.message, "bad");
        }
      }

      saveBtn.addEventListener("click", doSave);
      cancelBtn.addEventListener("click", paintView);
      input.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter") doSave();
        else if (ev.key === "Escape") paintView();
      });
    }

    paintView();
    return root;
  }

  async function refresh() {
    let cfg;
    try {
      cfg = await API.config();
    } catch (err) {
      console.warn("config refresh failed:", err);
      return;
    }

    const list = document.querySelector("#settings-list");
    if (!list) return;
    list.replaceChildren(...SETTINGS.map((def) => renderSetting(cfg, def)));
  }

  document.querySelector("#settings-refresh-btn")?.addEventListener("click", refresh);

  return { refresh };
})();
