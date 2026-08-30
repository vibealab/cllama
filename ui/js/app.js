// cllama web console — page logic and rendering.
//
// Depends on UI (js/component.js) and API (js/api.js).
//
// No polling: the page subscribes to GET /admin/events (SSE). The server
// emits a topic ("queue", "backends", "parents", or "hello" on connect)
// whenever relevant state changes, and we refetch just the matching
// admin endpoint in response.
(() => {
  "use strict";

  const $ = (sel) => document.querySelector(sel);
  const el = UI.el;
  const badge = UI.badge;

  function fmtWait(ms) {
    if (ms < 1000) return ms + " ms";
    const s = ms / 1000;
    if (s < 60) return s.toFixed(1) + " s";
    const m = Math.floor(s / 60);
    return m + "m " + String(Math.floor(s % 60)).padStart(2, "0") + "s";
  }

  // ── theme switch (🌞/🌙) ──────────────────────────────────────────────
  // Initial theme was already applied by the inline script in <head>
  // (stored choice, else system preference). The button only ever toggles
  // between dark and light and persists the explicit choice.
  const themeBtn = $("#theme-toggle");

  function applyTheme(theme, persist) {
    document.documentElement.dataset.theme = theme;
    themeBtn.textContent = theme === "light" ? "🌞" : "🌙";
    themeBtn.title = theme === "light" ? "Light theme — click to switch to dark" : "Dark theme — click to switch to light";
    if (persist) {
      try { localStorage.setItem("cllama-theme", theme); } catch (_) {}
    }
  }

  applyTheme(document.documentElement.dataset.theme || "dark", false);

  themeBtn.addEventListener("click", () => {
    applyTheme(document.documentElement.dataset.theme === "light" ? "dark" : "light", true);
  });

  // While no explicit choice is stored, keep following the OS live.
  const darkQuery = window.matchMedia("(prefers-color-scheme: light)");
  darkQuery.addEventListener?.("change", (ev) => {
    let stored = null;
    try { stored = localStorage.getItem("cllama-theme"); } catch (_) {}
    if (!stored) applyTheme(ev.matches ? "light" : "dark", false);
  });

  // ── tabs ───────────────────────────────────────────────────────────────
  for (const tab of document.querySelectorAll("#tabs .tab")) {
    tab.addEventListener("click", () => {
      document.querySelectorAll("#tabs .tab").forEach((t) => t.classList.toggle("active", t === tab));
      $("#tab-queue").classList.toggle("hidden", tab.dataset.tab !== "queue");
      $("#tab-models").classList.toggle("hidden", tab.dataset.tab !== "models");
    });
  }

  // ── tab 1: request queue ───────────────────────────────────────────────
  // Rows carry their server-side wait time plus the local time it was
  // fetched, so the "Waiting" column can tick without refetching.
  let queueFetchedAt = 0;

  // Lifecycle state → badge style for the queue table.
  const STATE_STYLE = { pending: "warn", takeaway: "muted", processing: "ok", done: "ok", fail: "bad" };

  async function refreshQueue() {
    let data;
    try {
      data = await API.queue();
    } catch (err) {
      console.warn("queue refresh failed:", err);
      return;
    }
    queueFetchedAt = Date.now();

    $("#stat-queued").textContent = data.queued;
    $("#stat-capacity").textContent = data.capacity === 0 ? "∞ off" : data.capacity;

    const tbody = $("#queue-rows");
    tbody.replaceChildren();
    for (const req of data.requests || []) {
      const tr = el("tr");
      const tdId = el("td", "mono");
      tdId.textContent = req.id;
      const tdModel = el("td", "mono");
      tdModel.textContent = req.model;
      const tdState = el("td");
      const stateBadge = badge(req.state || "?", STATE_STYLE[req.state] || "muted");
      if (req.error) stateBadge.title = req.error;
      tdState.appendChild(stateBadge);
      const tdBackend = el("td", "mono");
      tdBackend.textContent = req.backend || "—";
      const tdWait = el("td");
      tdWait.dataset.waitMs = req.wait_ms;
      tdWait.textContent = fmtWait(req.wait_ms);
      const tdAct = el("td", "actions-cell");
      tdAct.appendChild(iconButton("🗑️", "Remove from queue", () => removeQueued(req.id)));
      tr.append(tdId, tdModel, tdState, tdBackend, tdWait, tdAct);
      tbody.appendChild(tr);
    }
    $("#queue-empty").classList.toggle("hidden", (data.requests || []).length > 0);
  }

  async function removeQueued(id) {
    try {
      await API.removeQueued(id);
      UI.toast(`request ${id} removed from queue`);
    } catch (err) {
      UI.toast("remove failed: " + err.message, "bad");
    }
    refreshQueue();
  }

  // Local-only ticker: grows the displayed wait times between SSE events.
  setInterval(() => {
    if (!queueFetchedAt || document.querySelector("#tab-queue.hidden")) return;
    const drift = Date.now() - queueFetchedAt;
    for (const td of document.querySelectorAll("#queue-rows td[data-wait-ms]")) {
      td.textContent = fmtWait(Number(td.dataset.waitMs) + drift);
    }
  }, 1000);

  // ── tab 2: models ↔ backends ───────────────────────────────────────────
  const TYPE_LABEL = { ollama: "ollama", openai: "openai", tunnel: "child tunnel" };

  // Kept for the register dialog's model dropdown.
  let knownModels = [];

  async function refreshBackendsAndModels() {
    let backends, models;
    try {
      [backends, models] = await Promise.all([API.backends(), API.models()]);
    } catch (err) {
      console.warn("backend refresh failed:", err);
      return;
    }
    knownModels = models.models || [];

    renderModelMap(knownModels);
    renderBackends(backends.backends || []);
    $("#stat-models").textContent = knownModels.length;
  }

  function renderModelMap(models) {
    const root = $("#model-map");
    root.replaceChildren();
    if (models.length === 0) {
      root.appendChild(el("div", "empty", "No mock models configured (start cllama with -name model1,model2)."));
      return;
    }
    for (const m of models) {
      const group = el("div", "model-group");
      const title = el("div", "model-name");
      title.appendChild(document.createTextNode(m.name + " "));
      title.appendChild((m.backends || []).length
        ? badge("servable", "ok")
        : badge("no backend — requests queue", "warn"));
      group.appendChild(title);

      const table = el("table");
      const tbody = el("tbody");
      if ((m.backends || []).length === 0) {
        const tr = el("tr");
        const td = el("td");
        td.appendChild(el("span", "empty", "nothing bound — register a backend below"));
        tr.appendChild(td);
        tbody.appendChild(tr);
      }
      for (const b of m.backends || []) {
        const tr = el("tr");
        const tdId = el("td", "mono");
        tdId.textContent = b.id;
        const tdType = el("td");
        tdType.appendChild(badge(TYPE_LABEL[b.type] || b.type, b.type === "tunnel" ? "warn" : "muted"));
        const tdEp = el("td", "mono");
        tdEp.textContent = b.endpoint;
        const tdBind = el("td", "mono");
        tdBind.appendChild(el("span", "arrow", "→ "));
        tdBind.appendChild(el("span", "", b.upstream_model));
        const tdState = el("td");
        tdState.appendChild(b.disabled ? badge("disabled", "bad") : badge("enabled", "ok"));
        tr.append(tdId, tdType, tdEp, tdBind, tdState);
        tbody.appendChild(tr);
      }
      table.appendChild(tbody);
      group.appendChild(table);
      root.appendChild(group);
    }
  }

  // Icon action button (emoji glyph, meaning in the tooltip).
  function iconButton(glyph, title, handler) {
    const b = el("button", "icon-btn", glyph);
    b.type = "button";
    b.title = title;
    b.addEventListener("click", handler);
    return b;
  }

  function renderBackends(backends) {
    const tbody = $("#backend-rows");
    tbody.replaceChildren();
    for (const be of backends) {
      const tr = el("tr");

      const tdId = el("td", "mono");
      tdId.textContent = be.id;

      const tdType = el("td");
      tdType.appendChild(badge(TYPE_LABEL[be.type] || be.type, be.type === "tunnel" ? "warn" : "muted"));

      const tdEp = el("td", "mono");
      tdEp.textContent = be.endpoint;

      const tdBind = el("td", "mono");
      for (const b of be.bindings || []) {
        const line = el("div");
        line.textContent = b.model + " ";
        line.appendChild(el("span", "arrow", "→ "));
        line.appendChild(document.createTextNode(b.upstream_model));
        tdBind.appendChild(line);
      }

      const tdCaps = el("td");
      for (const c of be.capabilities || []) tdCaps.appendChild(badge(c, "muted"));

      const tdState = el("td");
      if (be.disabled) tdState.appendChild(badge("disabled", "bad"));
      else if (be.healthy) tdState.appendChild(badge("enabled", "ok"));
      else tdState.appendChild(badge("unhealthy", "warn"));
      if (be.has_token) tdState.appendChild(badge("token", "muted"));

      const tdAct = el("td", "actions-cell");
      tdAct.appendChild(iconButton(
        be.disabled ? "▶️" : "⏸️",
        be.disabled ? "Enable" : "Disable",
        () => setBackendEnabled(be.id, !!be.disabled),
      ));
      tdAct.appendChild(iconButton("⏹️", "Unregister", () => unregisterBackend(be.id, be.endpoint)));

      tr.append(tdId, tdType, tdEp, tdBind, tdCaps, tdState, tdAct);
      tbody.appendChild(tr);
    }
    $("#backend-empty").classList.toggle("hidden", backends.length > 0);
  }

  async function setBackendEnabled(id, enabled) {
    try {
      await API.setBackendEnabled(id, enabled);
      UI.toast(`backend ${id} ${enabled ? "enabled" : "disabled"}`);
    } catch (err) {
      UI.toast("toggle failed: " + err.message, "bad");
    }
    // The server also emits a "backends" event; refetch for immediacy.
    refreshBackendsAndModels();
  }

  async function unregisterBackend(id, endpoint) {
    const ok = await UI.confirm({
      title: "Unregister backend",
      message: `Unregister ${id} (${endpoint})? Queued requests for its models will wait for another backend.`,
      okLabel: "Unregister",
      danger: true,
    });
    if (!ok) return;
    try {
      await API.unregisterBackend(id);
      UI.toast(`backend ${id} unregistered`);
    } catch (err) {
      UI.toast("unregister failed: " + err.message, "bad");
    }
    refreshBackendsAndModels();
  }

  // ── register backend dialog ─────────────────────────────────────────────
  function field(labelText, inputNode, optText) {
    const label = el("label");
    label.appendChild(document.createTextNode(labelText + " "));
    if (optText) label.appendChild(el("span", "opt", optText));
    label.appendChild(inputNode);
    return label;
  }

  function openRegisterDialog() {
    const typeSel = el("select");
    typeSel.name = "type";
    typeSel.append(el("option", null, "ollama"), el("option", null, "openai"));

    const endpointIn = el("input");
    endpointIn.name = "endpoint";
    endpointIn.placeholder = "http://127.0.0.1:11434";
    endpointIn.required = true;

    const tokenIn = el("input");
    tokenIn.name = "token";
    tokenIn.type = "password";
    tokenIn.placeholder = "bearer token";

    const modelSel = el("select");
    modelSel.name = "model";
    for (const m of knownModels) modelSel.appendChild(el("option", null, m.name));

    const upstreamIn = el("input");
    upstreamIn.name = "upstream_model";
    upstreamIn.placeholder = "llama3:8b";

    const capsIn = el("input");
    capsIn.name = "capabilities";
    capsIn.placeholder = "completion,vision,tools,thinking";

    const error = el("div", "form-error hidden");

    const form = el("form", "form-col");
    form.append(
      field("Type", typeSel),
      field("Endpoint", endpointIn),
      field("Token", tokenIn, "optional"),
      field("Model", modelSel),
      field("Upstream model", upstreamIn, "defaults to model"),
      field("Capabilities", capsIn, "empty = default"),
      error,
    );

    const dlg = UI.modal({
      title: "Register backend",
      content: form,
      actions: [
        { label: "Cancel", onClick: (close) => close() },
        { label: "Register", className: "primary", onClick: () => form.requestSubmit() },
      ],
    });

    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const fd = new FormData(form);
      const body = {
        type: fd.get("type"),
        endpoint: String(fd.get("endpoint") || "").trim(),
        token: String(fd.get("token") || "").trim(),
        model: fd.get("model"),
        upstream_model: String(fd.get("upstream_model") || "").trim(),
      };
      const caps = String(fd.get("capabilities") || "").split(",").map((c) => c.trim()).filter(Boolean);
      if (caps.length) body.capabilities = caps;

      error.classList.add("hidden");
      try {
        const created = await API.registerBackend(body);
        dlg.close();
        UI.toast(`registered backend ${created.id}`);
      } catch (err) {
        error.textContent = err.message;
        error.classList.remove("hidden");
      }
      refreshBackendsAndModels();
    });
  }

  $("#register-backend-btn").addEventListener("click", openRegisterDialog);

  // ── parent connections (child side) ────────────────────────────────────
  async function refreshParents() {
    let data;
    try {
      data = await API.parents();
    } catch (err) {
      console.warn("parents refresh failed:", err);
      return;
    }
    const parents = data.parents || [];
    const tbody = $("#parent-rows");
    tbody.replaceChildren();
    for (const p of parents) {
      const tr = el("tr");
      const tdId = el("td", "mono");
      tdId.textContent = p.id;
      const tdParent = el("td", "mono");
      tdParent.textContent = p.parent;
      const tdModel = el("td", "mono");
      tdModel.textContent = p.model;
      const tdTo = el("td", "mono");
      tdTo.textContent = p.to_model || "—";
      const tdState = el("td");
      tdState.appendChild(p.connected ? badge("connected", "ok") : badge("down", "bad"));
      if (p.last_error) tdState.appendChild(badge(p.last_error, "muted"));

      const tdAct = el("td", "actions-cell");
      tdAct.appendChild(iconButton("🔌", "Disconnect", async () => {
        const ok = await UI.confirm({
          title: "Disconnect parent",
          message: `Remove the tunnel to ${p.parent} for model ${p.model}? The parent will stop routing that model through this server.`,
          okLabel: "Disconnect",
          danger: true,
        });
        if (!ok) return;
        try {
          await API.disconnectParent(p.id);
          UI.toast("parent connection removed");
        } catch (err) {
          UI.toast("disconnect failed: " + err.message, "bad");
        }
        refreshParents();
      }));

      tr.append(tdId, tdParent, tdModel, tdTo, tdState, tdAct);
      tbody.appendChild(tr);
    }
    $("#parent-empty").classList.toggle("hidden", parents.length > 0);
  }

  function openConnectParentDialog() {
    const urlIn = el("input");
    urlIn.name = "url";
    urlIn.placeholder = "http://token@parent.example.com:11434/model?to=local-model";
    urlIn.required = true;

    const error = el("div", "form-error hidden");

    const form = el("form", "form-col");
    form.append(field("Parent URL", urlIn), error);

    const dlg = UI.modal({
      title: "Connect to parent cllama server",
      content: form,
      actions: [
        { label: "Cancel", onClick: (close) => close() },
        { label: "Connect", className: "primary", onClick: () => form.requestSubmit() },
      ],
    });

    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const url = String(new FormData(form).get("url") || "").trim();
      error.classList.add("hidden");
      try {
        await API.connectParent(url);
        dlg.close();
        UI.toast("connecting to parent…");
      } catch (err) {
        error.textContent = err.message;
        error.classList.remove("hidden");
      }
      refreshParents();
    });
  }

  $("#connect-parent-btn").addEventListener("click", openConnectParentDialog);

  // ── SSE live updates ───────────────────────────────────────────────────
  const statusEl = $("#conn-status");

  const es = API.events((topic) => {
    switch (topic) {
      case "hello":
        refreshQueue();
        refreshBackendsAndModels();
        refreshParents();
        break;
      case "queue":
        refreshQueue();
        break;
      case "backends":
        refreshBackendsAndModels();
        break;
      case "parents":
        refreshParents();
        break;
    }
  });
  es.onopen = () => {
    statusEl.textContent = "● live";
    statusEl.className = "conn-status live";
  };
  es.onerror = () => {
    // EventSource reconnects by itself; nothing else to do.
    statusEl.textContent = "● reconnecting…";
    statusEl.className = "conn-status dead";
  };
})();
