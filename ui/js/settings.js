// cllama web console — settings tab.
//
// Renders the server's in-memory system configuration (GET /admin/config):
// queue depth, done/failed retention, the generation timeout, and the two
// auth tokens. app.js drives refresh() on "hello" and "config" SSE topics;
// the tab also has a manual refresh.
//
// Tokens display masked (••••): the 👁 button reveals the plain text
// (fetched lazily from /admin/config/secret/<name>) and 🙈 masks it again;
// ✏️ opens a dialog to change or clear the token.
// Exposes a single global: Settings.
window.Settings = (() => {
  "use strict";

  const el = UI.el;
  const badge = UI.badge;

  // Retention values are whole seconds on the wire:
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

  const ROWS = [
    {
      key: "max_queue",
      name: "Max queue size",
      desc: "Requests held while no backend serves their model (0 disables queueing).",
      render: (v) => (v === 0 ? badge("queueing off", "muted") : String(v)),
    },
    {
      key: "done_retention_sec",
      name: "Done retention",
      desc: "How long completed requests stay in the queue: -1 forever (manual removal), 0 no stay (default), >0 seconds to keep.",
      render: fmtRetention,
    },
    {
      key: "failed_retention_sec",
      name: "Failed retention",
      desc: "Same semantics for failed requests.",
      render: fmtRetention,
    },
    {
      key: "gen_timeout_sec",
      name: "Generation timeout",
      desc: "Abort an upstream generation request after this many seconds (0 = no timeout). Applies to clients of backends registered after a change.",
      render: fmtTimeout,
    },
  ];

  // Auth tokens: masked by default, 👁 reveals, ✏️ changes.
  const SECRET_ROWS = [
    {
      key: "api_token",
      flag: "has_api_token",
      label: "API token",
      desc: "Bearer token required on LLM API and model-list requests (seeds from -token). Empty disables auth; changes apply immediately.",
    },
    {
      key: "parent_auth",
      flag: "has_parent_auth",
      label: "Parent auth token",
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

  function openSecretDialog(key, label) {
    const input = el("input");
    input.type = "text";
    input.autocomplete = "new-password";
    input.spellcheck = false;
    input.placeholder = "new token (empty disables auth)";

    UI.modal({
      title: "Change " + label,
      content: input,
      actions: [
        { label: "Cancel", onClick: (close) => close() },
        {
          label: "Save",
          className: "primary",
          onClick: async (close) => {
            try {
              await API.updateConfig({ [key]: input.value });
              UI.toast(label + " updated");
              close();
            } catch (err) {
              UI.toast("update failed: " + err.message, "bad");
            }
            refresh();
          },
        },
      ],
    });
    input.focus();
  }

  function renderSecretRow(cfg, row) {
    const tr = el("tr");

    const tdName = el("td");
    tdName.textContent = row.label;

    const tdValue = el("td", "mono");
    const set = !!cfg[row.flag];
    let revealed = null; // cached plain text while 👁 is active; null = masked

    async function toggle() {
      if (revealed !== null) {
        revealed = null; // mask again
        paint();
        return;
      }
      try {
        const data = await API.configSecret(row.key);
        revealed = data.value || "";
      } catch (err) {
        UI.toast("reveal failed: " + err.message, "bad");
        return;
      }
      paint();
    }

    function paint() {
      const kids = [];
      if (!set) {
        kids.push(badge("no token (auth off)", "muted"));
      } else {
        const display = el("span");
        display.textContent = revealed === null ? "••••••••" : revealed || "(empty)";
        kids.push(display);
        kids.push(document.createTextNode(" "));
        kids.push(iconButton(revealed === null ? "👁️" : "🙈", revealed === null ? "Reveal" : "Hide", toggle));
      }
      kids.push(document.createTextNode(" "));
      kids.push(iconButton("✏️", set ? "Change" : "Set", () => openSecretDialog(row.key, row.label)));
      tdValue.replaceChildren(...kids);
    }
    paint();

    const tdDesc = el("td", "hint");
    tdDesc.textContent = row.desc;

    tr.append(tdName, tdValue, tdDesc);
    return tr;
  }

  async function refresh() {
    let cfg;
    try {
      cfg = await API.config();
    } catch (err) {
      console.warn("config refresh failed:", err);
      return;
    }

    const tbody = document.querySelector("#settings-rows");
    if (!tbody) return;
    tbody.replaceChildren();
    for (const row of ROWS) {
      const tr = el("tr");

      const tdName = el("td");
      tdName.textContent = row.name;

      const tdValue = el("td");
      const rendered = row.render(cfg[row.key]);
      if (rendered instanceof Node) tdValue.appendChild(rendered);
      else tdValue.appendChild(document.createTextNode(String(rendered)));

      const tdDesc = el("td", "hint");
      tdDesc.textContent = row.desc;

      tr.append(tdName, tdValue, tdDesc);
      tbody.appendChild(tr);
    }
    for (const row of SECRET_ROWS) tbody.appendChild(renderSecretRow(cfg, row));
  }

  document.querySelector("#settings-refresh-btn")?.addEventListener("click", refresh);

  return { refresh };
})();
