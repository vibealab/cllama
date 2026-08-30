// cllama web console — settings tab.
//
// Renders the server's in-memory system configuration (GET /admin/config):
// queue depth and the done/failed request retention. app.js drives refresh()
// on "hello" and "config" SSE topics; the tab also has a manual refresh.
// Exposes a single global: Settings.
window.Settings = (() => {
  "use strict";

  // Retention values are whole seconds on the wire:
  // -1 = stay in the queue forever (manual removal only),
  //  0 = leave the queue immediately,
  // >0 = stay in the queue that long, then get purged.
  function fmtRetention(sec) {
    if (sec === -1) return UI.badge("forever (manual removal)", "warn");
    if (sec === 0) return UI.badge("0 s (no stay in queue)", "muted");
    return UI.badge(sec + " s", "ok");
  }

  // Generation timeout: 0 = no timeout, >0 = abort upstream after N seconds.
  function fmtTimeout(sec) {
    if (sec <= 0) return UI.badge("no timeout", "muted");
    return UI.badge(sec + " s", "ok");
  }

  const ROWS = [
    {
      key: "max_queue",
      name: "Max queue size",
      desc: "Requests held while no backend serves their model (0 disables queueing).",
      render: (v) => (v === 0 ? UI.badge("queueing off", "muted") : String(v)),
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
      const tr = UI.el("tr");

      const tdName = UI.el("td");
      tdName.textContent = row.name;

      const tdValue = UI.el("td");
      const rendered = row.render(cfg[row.key]);
      if (rendered instanceof Node) tdValue.appendChild(rendered);
      else tdValue.appendChild(document.createTextNode(String(rendered)));

      const tdDesc = UI.el("td", "hint");
      tdDesc.textContent = row.desc;

      tr.append(tdName, tdValue, tdDesc);
      tbody.appendChild(tr);
    }
  }

  document.querySelector("#settings-refresh-btn")?.addEventListener("click", refresh);

  return { refresh };
})();
