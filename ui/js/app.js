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
    if (ms < 1000) return ms + "ms";
    const s = Math.floor(ms / 1000);
    if (s < 60) return s + "s";
    const m = Math.floor(s / 60);
    if (m < 60) return m + "m" + String(s % 60).padStart(2, "0") + "s";
    const h = Math.floor(m / 60);
    return h + "h" + String(m % 60).padStart(2, "0") + "m";
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
      for (const panel of document.querySelectorAll("main .panel")) {
        panel.classList.toggle("hidden", panel.id !== "tab-" + tab.dataset.tab);
      }
    });
  }

  // ── tab 1: request queue ───────────────────────────────────────────────
  // Rows carry their server-side timings plus the local time they were
  // fetched, so the timing column can tick without refetching:
  // in-flight requests show their live processing time; finished ones show
  // the frozen processing time plus the ticking retention time,
  // e.g. "5m21s (56s)".
  let queueFetchedAt = 0;

  // Lifecycle state → badge style for the queue table.
  const STATE_STYLE = { pending: "warn", takeaway: "muted", processing: "ok", done: "ok", fail: "bad" };

  // renderWait (re)draws one timing cell from its dataset, accounting for
  // the drift since the last queue fetch. Used on render and by the ticker.
  function renderWait(td) {
    const drift = Date.now() - queueFetchedAt;
    const wait = Number(td.dataset.waitMs);
    if (td.dataset.endedMsAgo === undefined) {
      td.textContent = fmtWait(wait + drift); // still in flight
      return;
    }
    td.textContent = fmtWait(wait) + " (" + fmtWait(Number(td.dataset.endedMsAgo) + drift) + ")";
  }

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
      if (req.ended_ms_ago !== undefined && req.ended_ms_ago !== null) {
        tdWait.dataset.endedMsAgo = req.ended_ms_ago;
        tdWait.title = "finished " + fmtWait(req.ended_ms_ago) + " ago; purged after the retention period";
      } else {
        tdWait.title = "processing time so far";
      }
      renderWait(tdWait);
      const tdAct = el("td", "actions-cell");
      if (req.state === "pending") {
        tdAct.appendChild(iconButton("✋", "Take away (manual handling) and respond", () => takeoverAndRespond(req)));
      } else if (req.state === "takeaway" && req.manual) {
        tdAct.appendChild(iconButton("✋", "Open manual response dialog", () => openRespondDialog(req)));
      }
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

  // ── manual handling (takeaway flow) ──────────────────────────────────
  // Pin a pending request (the selector stops routing it) and open the
  // respond dialog. Cancelling the dialog marks the request pending again.
  async function takeoverAndRespond(req) {
    try {
      await API.takeoverQueued(req.id);
    } catch (err) {
      UI.toast("takeover failed: " + err.message, "bad");
      refreshQueue();
      return;
    }
    refreshQueue();
    openRespondDialog(req);
  }

  function openRespondDialog(req) {
    // Once an answer has been sent, closing must not release the request.
    let settled = false;

    const error = el("div", "form-error hidden");
    const showError = (msg) => {
      error.textContent = msg;
      error.classList.remove("hidden");
    };

    const answer = el("textarea");
    answer.rows = 8;
    answer.placeholder = "Type the response to send to the client…";

    const backendSel = el("select");
    const proxyBtn = iconButton("📡", "Proxy the request to the selected backend and draft a response", () => proxyToBackend());
    const proxyRow = el("div", "respond-proxy-row");
    proxyRow.append(backendSel, proxyBtn);

    const body = el("div", "form-col");
    body.append(
      field("Response", answer, "sent to the client as typed"),
      field("Draft with backend", proxyRow, "proxies the original request into the text area"),
      error,
    );

    // Offer every chat-capable backend (embedding-only ones cannot answer).
    async function loadBackends() {
      try {
        const data = await API.backends();
        const chat = (data.backends || []).filter((be) => {
          const caps = be.capabilities || [];
          return caps.length === 0 || caps.includes("completion");
        });
        backendSel.replaceChildren();
        if (!chat.length) {
          const opt = el("option", null, "no chat backends registered");
          opt.value = "";
          opt.disabled = true;
          backendSel.appendChild(opt);
          return;
        }
        for (const be of chat) {
          const opt = el("option", null, `${be.id} · ${be.type} · ${be.endpoint}${be.disabled ? " (disabled)" : ""}`);
          opt.value = be.id;
          backendSel.appendChild(opt);
        }
      } catch (err) {
        showError("loading backends failed: " + err.message);
      }
    }

    async function proxyToBackend() {
      if (!backendSel.value) {
        showError("Select a backend to proxy to first.");
        return;
      }
      error.classList.add("hidden");
      proxyBtn.disabled = true;
      proxyBtn.textContent = "⏳";
      try {
        const data = await API.proxyQueued(req.id, backendSel.value);
        answer.value = data.content || "";
        answer.focus();
      } catch (err) {
        showError("proxy failed: " + err.message);
      } finally {
        proxyBtn.disabled = false;
        proxyBtn.textContent = "📡";
      }
    }

    async function send(btn) {
      const text = answer.value;
      if (!text.trim()) {
        showError("The response is empty.");
        return;
      }
      error.classList.add("hidden");
      btn.disabled = true;
      try {
        await API.resolveQueued(req.id, text);
        settled = true; // closing below must not release the request
        UI.toast(`response sent to the client for ${req.id}`);
        dlg.close();
      } catch (err) {
        btn.disabled = false;
        showError("send failed: " + err.message);
      }
    }

    const dlg = UI.modal({
      title: `Respond to ${req.id}${req.model ? " · " + req.model : ""}`,
      content: body,
      actions: [
        { label: "Cancel", onClick: (close) => close() },
        { label: "📤 Send", className: "primary", onClick: (close, btn) => send(btn) },
      ],
      // Cancel (also ✕, mask click or Escape): unless an answer was sent,
      // the request goes back to pending so the selector can route it again.
      onClose: async () => {
        if (settled) {
          refreshQueue();
          return;
        }
        try {
          await API.releaseQueued(req.id);
        } catch (_) { /* the entry may already be gone (client left) */ }
        refreshQueue();
      },
    });
    loadBackends();
    answer.focus();
  }

  // Local-only ticker: grows the displayed times between SSE events —
  // processing time for in-flight requests, retention time for finished ones.
  setInterval(() => {
    if (!queueFetchedAt || document.querySelector("#tab-queue.hidden")) return;
    for (const td of document.querySelectorAll("#queue-rows td[data-wait-ms]")) {
      renderWait(td);
    }
  }, 1000);

  // ── tab 2: models ↔ backends ───────────────────────────────────────────
  const TYPE_LABEL = { ollama: "ollama", openai: "openai", anthropic: "anthropic", tunnel: "child tunnel" };

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
    typeSel.append(
      el("option", null, "ollama"),
      el("option", null, "openai"),
      el("option", null, "anthropic"),
    );

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
        Settings.refresh();
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
      case "config":
        Settings.refresh();
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
