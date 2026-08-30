// cllama admin API client.
//
// Wraps every endpoint the dashboard talks to plus the /admin/events SSE
// stream. Exposes a single global: API.
window.API = (() => {
  "use strict";

  async function getJSON(path) {
    const res = await fetch(path, { cache: "no-store" });
    if (!res.ok) throw new Error(path + " -> HTTP " + res.status);
    return res.json();
  }

  async function sendJSON(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let data = {};
    try { data = await res.json(); } catch (_) {}
    if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
    return data;
  }

  const enc = encodeURIComponent;

  return {
    // Reads
    queue:      () => getJSON("/admin/queue"),
    models:     () => getJSON("/admin/models"),
    backends:   () => getJSON("/admin/backends"),
    parents:    () => getJSON("/admin/parents"),

    // Backends
    registerBackend:   (backend) => sendJSON("POST", "/admin/backends", backend),
    setBackendEnabled: (id, enabled) => sendJSON("POST", `/admin/backends/${enc(id)}/enabled`, { enabled }),
    unregisterBackend: (id) => sendJSON("DELETE", `/admin/backends/${enc(id)}`),

    // Parent connections (child side)
    connectParent:    (url) => sendJSON("POST", "/admin/parents", { url }),
    disconnectParent: (id) => sendJSON("DELETE", `/admin/parents/${enc(id)}`),

    // Request queue
    removeQueued: (id) => sendJSON("DELETE", `/admin/queue/${enc(id)}`),

    // Live updates: onTopic receives "hello" | "queue" | "backends" |
    // "parents". Returns the EventSource so callers can hook open/error.
    events: (onTopic) => {
      const es = new EventSource("/admin/events");
      es.onmessage = (ev) => onTopic(ev.data, es);
      return es;
    },
  };
})();
