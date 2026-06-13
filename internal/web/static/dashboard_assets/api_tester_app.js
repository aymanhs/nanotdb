(function () {
  const API_BASE = "";

  // All available endpoints
  const ENDPOINTS = [
    {
      id: "health",
      method: "GET",
      path: "/health",
      title: "Health Check",
      description: "Check if the NanoTDB server is up and running.",
      params: [],
    },
    {
      id: "databases",
      method: "GET",
      path: "/api/v1/databases",
      title: "List Databases",
      description: "List all user-created databases. Add ?include_internal=true to include the internal stats database.",
      params: [
        { name: "include_internal", type: "boolean", required: false, description: "Include internal stats database" },
      ],
    },
    {
      id: "metrics",
      method: "GET",
      path: "/api/v1/metrics",
      title: "List Metrics",
      description: "List all metrics in a specific database. Optionally include metric IDs and types with details=true.",
      params: [
        { name: "db", type: "string", required: true, description: "Database name (e.g., mydb)" },
        { name: "details", type: "boolean", required: false, description: "Include metric ID and type information" },
      ],
    },
    {
      id: "aggregates",
      method: "GET",
      path: "/api/v1/aggregates",
      title: "List Aggregates",
      description: "Get the list of supported aggregate functions for windowed queries.",
      params: [],
    },
    {
      id: "query",
      method: "GET",
      path: "/api/v1/query",
      title: "Instant Query",
      description: "Fetch the latest value for a metric. Returns a single scalar result with the current timestamp and value.",
      params: [{ name: "query", type: "string", required: true, description: "Metric name (format: database/metric.name)" }],
    },
    {
      id: "query_range",
      method: "GET",
      path: "/api/v1/query_range",
      title: "Range Query",
      description: "Fetch a time series of raw data points. Results are sampled at the specified step interval.",
      params: [
        { name: "query", type: "string", required: true, description: "Metric name (format: database/metric.name)", sample: "metrics/temp.cpu" },
        { name: "start", type: "string", required: true, description: "Start time (RFC3339, Unix seconds/ns, or negative duration like -5m). Max lookback is 10y.", sample: "-5m" },
        { name: "end", type: "string", required: true, description: "End time (RFC3339, Unix seconds/ns, or negative duration like -10s)", sample: "-0s" },
        { name: "step", type: "string", required: true, description: "Sampling stride (e.g., 60s, 5m, 1h). Note: Queries are capped at 100,000 points." },
        { name: "timestamp_unit", type: "string", required: false, description: "Timestamp unit (ns, us, ms, s; default ns)" },
      ],
    },
    {
      id: "query_range_aggregate",
      method: "GET",
      path: "/api/v1/query_range",
      title: "Aggregate Query",
      description: "Fetch aggregated metrics over time windows. Returns windowed statistics like min, max, avg, count per bucket.",
      params: [
        { name: "query", type: "string", required: true, description: "Metric name (format: database/metric.name)" },
        { name: "start", type: "string", required: true, description: "Start time (RFC3339, Unix seconds/ns, or negative duration like -5m)" },
        { name: "end", type: "string", required: true, description: "End time (RFC3339, Unix seconds/ns, or negative duration like -10s)" },
        { name: "aggregate", type: "string", required: true, description: "Comma-separated aggregates (e.g., min,max,avg,count)" },
        { name: "window", type: "string", required: true, description: "Window duration (e.g., 5m, 1h)" },
        { name: "timestamp_unit", type: "string", required: false, description: "Timestamp unit (ns, us, ms, s; default ns)" },
      ],
    },
    {
      id: "import",
      method: "POST",
      path: "/api/v1/import",
      title: "Import Line Protocol",
      description: "Ingest time-series data in line protocol format. One sample per line: database/metric.name value [timestamp_ns]. Timestamps are optional; current time is used if omitted.",
      params: [
        { name: "body", type: "textarea", required: true, description: "Line protocol data (one metric per line)", sample: "metrics/temp.cpu 22.5\nmetrics/cpu.busy 0.15" },
      ],
    },
    {
      id: "rollup_backfill",
      method: "POST",
      path: "/api/v1/rollup/backfill",
      title: "Rollup Backfill",
      description: "Rebuild rollup destination databases from source data. Provide source_db, source_dbs array, or omit to backfill all discovered sources.",
      params: [
        { name: "body", type: "textarea", required: true, description: 'JSON body (e.g., {"source_db":"mydb"} or {"source_dbs":["db1","db2"]} or {})', sample: '{"source_db": "metrics"}' },
      ],
    },
    {
      id: "events_create",
      method: "POST",
      path: "/api/v1/events",
      title: "Create Events",
      description: "Ingest events as JSON array or single object. Each event has: db (required), name (required), ts (optional), value (optional, int32/float32), payload (optional).",
      params: [
        { name: "db", type: "string", required: true, description: "Database name" },
        { name: "body", type: "textarea", required: true, description: 'JSON array or object: [{"name":"event.name","value":123,"payload":{...}}]', sample: '[{"name": "app.boot", "value": 1, "payload": {"version": "1.5.0"}}]' },
      ],
    },
    {
      id: "events_query",
      method: "GET",
      path: "/api/v1/events",
      title: "Query Events",
      description: "Range query for events with optional name filter. Returns up to limit events (default 100, max 1000).",
      params: [
        { name: "db", type: "string", required: true, description: "Database name" },
        { name: "start", type: "string", required: true, description: "Start time (RFC3339, Unix seconds/ns, or negative duration like -5m)" },
        { name: "end", type: "string", required: false, description: "End time (RFC3339, Unix seconds/ns, or negative duration like -10s; defaults to now)" },
        { name: "name", type: "string", required: false, description: "Event name filter (exact match or wildcard: *, ?, [abc])" },
        { name: "limit", type: "string", required: false, description: "Max events to return (default 100, max 1000)" },
      ],
    },
    {
      id: "events_aggregate",
      method: "GET",
      path: "/api/v1/events/aggregate",
      title: "Aggregate Events (Count)",
      description: "Time-bucketed count of matching events. Returns event count per bucket.",
      params: [
        { name: "db", type: "string", required: true, description: "Database name" },
        { name: "start", type: "string", required: true, description: "Start time (RFC3339, Unix seconds/ns, or negative duration like -5m)" },
        { name: "end", type: "string", required: false, description: "End time (RFC3339, Unix seconds/ns, or negative duration like -10s; defaults to now)" },
        { name: "window", type: "string", required: true, description: "Bucket size (e.g., 5m, 1h, 1d)" },
        { name: "name", type: "string", required: false, description: "Event name filter (exact match or wildcard)" },
        { name: "timestamp_unit", type: "string", required: false, description: "Timestamp unit (ns, us, ms, s; default ns)" },
      ],
    },
    {
      id: "events_catalog",
      method: "GET",
      path: "/api/v1/events/catalog",
      title: "Event Catalog",
      description: "List all registered event names, IDs, and value types for a database.",
      params: [
        { name: "db", type: "string", required: true, description: "Database name" },
      ],
    },
    {
      id: "internal_events_catalog",
      method: "GET",
      path: "/api/v1/internal-events/catalog",
      title: "Internal Events Catalog",
      description: "List the authoritative registry of internal events (lifecycle, engine, drip) even before they are emitted.",
      params: [],
    },
    {
      id: "internal_events_groups",
      method: "GET",
      path: "/api/v1/internal-events/groups",
      title: "Internal Events Groups",
      description: "Get the current enablement state and source (config vs runtime) for all internal event groups.",
      params: [],
    },
    {
      id: "internal_events_groups_set",
      method: "POST",
      path: "/api/v1/internal-events/groups",
      title: "Set Internal Event Groups",
      description: "Toggle internal event groups on or off at runtime. Changes do not persist across restarts.",
      params: [
        { name: "body", type: "textarea", required: true, description: 'JSON mapping group names to "on" or "off"', sample: '{"nanotdb.wal": "on", "nanotdb.wal.fsync": "on"}' },
      ],
    },
  ];

  let currentEndpoint = null;

  function apiURL(path, params) {
    let url = API_BASE + path;
    if (params && Object.keys(params).length > 0) {
      const query = new URLSearchParams();
      for (const [key, value] of Object.entries(params)) {
        if (value !== undefined && value !== null && value !== "") {
          query.append(key, value);
        }
      }
      const qs = query.toString();
      if (qs) url += "?" + qs;
    }
    return url;
  }

  function renderEndpointsList() {
    const container = document.getElementById("endpointsList");
    container.innerHTML = "";

    ENDPOINTS.forEach((endpoint) => {
      const btn = document.createElement("button");
      btn.className = "endpoint-btn";
      btn.innerHTML = `<span class="method-badge method-${endpoint.method.toLowerCase()}">${endpoint.method}</span> ${endpoint.title}`;
      btn.addEventListener("click", () => selectEndpoint(endpoint));
      container.appendChild(btn);
    });
  }

  function selectEndpoint(endpoint) {
    currentEndpoint = endpoint;

    // Update active button
    document.querySelectorAll(".endpoint-btn").forEach((btn) => {
      btn.classList.remove("active");
    });
    event.target.closest(".endpoint-btn").classList.add("active");

    renderEndpointContent();
  }

  function renderEndpointContent() {
    if (!currentEndpoint) return;

    const container = document.getElementById("endpointContent");
    container.innerHTML = "";

    // Inject compact styles for left-aligned labels and tooltips
    const styleId = "api-tester-compact-styles";
    if (!document.getElementById(styleId)) {
      const style = document.createElement("style");
      style.id = styleId;
      style.textContent = `
        .endpoint-form { padding: 0.5rem 0; }
        .form-group { display: flex; align-items: center; margin-bottom: 0.5rem !important; gap: 1rem; }
        .form-label, .form-label-checkbox { width: 160px; flex-shrink: 0; margin-bottom: 0 !important; text-align: right; font-size: 13px; font-weight: 500; color: #94a3b8; }
        .form-input, .form-textarea { flex-grow: 1; padding: 4px 8px !important; margin: 0; background: #1e293b; border: 1px solid #334155; color: #f8fafc; border-radius: 4px; }
        .form-input:focus, .form-textarea:focus { outline: none; border-color: #38bdf8; background: #0f172a; }
        /* Prevent browser autofill from breaking the dark theme */
        input:-webkit-autofill,
        input:-webkit-autofill:hover, 
        input:-webkit-autofill:focus,
        textarea:-webkit-autofill,
        textarea:-webkit-autofill:hover,
        textarea:-webkit-autofill:focus {
          -webkit-text-fill-color: #f8fafc !important;
          -webkit-box-shadow: 0 0 0px 1000px #1e293b inset !important;
          transition: background-color 5000s ease-in-out 0s;
        }
        .form-checkbox { flex-grow: 0; margin-right: auto; }
        .form-textarea { height: 80px; resize: vertical; }
        .response-viewer { max-height: 700px; overflow-y: auto; border: 1px solid #333; border-radius: 4px; background: #0f172a; }
        .response-viewer pre { margin: 0; padding: 1rem; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 13px; line-height: 1.5; color: #f8fafc; white-space: pre-wrap; word-break: break-all; }
        .url-preview-input { width: 100%; padding: 8px 12px; background: #1e293b; border: 1px solid #334155; border-radius: 4px; color: #38bdf8; font-family: monospace; font-size: 13px; margin-bottom: 0.5rem; }
        .json-key { color: #81a2be; font-weight: 500; }
        .json-string { color: #b5bd68; }
        .json-number { color: #de935f; }
        .json-boolean { color: #b294bb; }
        .json-null { color: #cc6666; }
        .response-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 0.5rem; font-weight: 600; color: #94a3b8; font-size: 14px; }
        .response-status { font-weight: 600; font-size: 12px; padding: 2px 10px; border-radius: 12px; }
        .response-status.success { background: #064e3b; color: #34d399; }
        .response-status.error { background: #7f1d1d; color: #f87171; }
      `;
      document.head.appendChild(style);
    }

    const ep = currentEndpoint;

    // Header
    const header = document.createElement("div");
    header.className = "endpoint-header";
    header.innerHTML = `
      <div class="endpoint-title">
        <span class="method-badge method-${ep.method.toLowerCase()}">${ep.method}</span>
        <span>${ep.title}</span>
      </div>
      <p class="endpoint-description">${ep.description}</p>
    `;

    // Body
    const body = document.createElement("div");
    body.className = "endpoint-body";

    // URL Preview (before form)
    const urlPreview = document.createElement("div");
    urlPreview.className = "url-preview-section";
    urlPreview.innerHTML = `
      <input type="text" class="url-preview-input" id="urlPreviewInput" readonly />
    `;
    body.appendChild(urlPreview);

    // Form
    const form = document.createElement("form");
    form.className = "endpoint-form";
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      executeRequest(form);
    });

    ep.params.forEach((param) => {
      const group = document.createElement("div");
      group.className = "form-group";

      let input;
      if (param.type === "textarea") {
        const label = document.createElement("label");
        label.className = "form-label";
        label.innerHTML = `${param.name}${param.required ? '<span class="required">*</span>' : ""}`;

        input = document.createElement("textarea");
        input.className = "form-textarea";
        input.placeholder = `Enter ${param.name}...`;
        if (param.sample) {
          input.value = param.sample;
        }

        group.appendChild(label);
        group.appendChild(input);
      } else if (param.type === "boolean") {
        const label = document.createElement("label");
        label.className = "form-label-checkbox";
        label.htmlFor = `param_${param.name}`;
        label.textContent = param.name;

        input = document.createElement("input");
        input.type = "checkbox";
        input.className = "form-checkbox";
        input.id = `param_${param.name}`;
        input.name = param.name;
        input.dataset.type = param.type;

        group.appendChild(label);
        group.appendChild(input);
      } else {
        const label = document.createElement("label");
        label.className = "form-label";
        label.innerHTML = `${param.name}${param.required ? '<span class="required">*</span>' : ""}`;

        input = document.createElement("input");
        input.type = "text";
        input.className = "form-input";
        input.placeholder = `Enter ${param.name}...`;

        group.appendChild(label);
        group.appendChild(input);
      }

      if (!input.id) {
        input.id = `param_${param.name}`;
      }
      if (!input.name) {
        input.name = param.name;
      }
      if (!input.dataset.type) {
        input.dataset.type = param.type;
      }

      // Use title as tooltip instead of hint text
      input.title = param.description;

      form.appendChild(group);
    });

    const actions = document.createElement("div");
    actions.className = "form-actions";
    actions.innerHTML = `
      <button type="submit" class="btn btn-primary">Execute</button>
    `;

    form.appendChild(actions);

    // Update URL preview on input change
    const inputs = form.querySelectorAll("input[name], textarea");
    inputs.forEach((input) => {
      input.addEventListener("change", () => updateUrlPreview(form));
      input.addEventListener("input", () => updateUrlPreview(form));
    });

    // Response section
    const responseSection = document.createElement("div");
    responseSection.className = "response-section";
    responseSection.innerHTML = `
      <div class="response-header">
        <span>Response</span>
        <div id="responseStatus"></div>
      </div>
      <div class="response-viewer" id="responseViewer">
        <div class="empty-response">No response yet. Execute the request to see results.</div>
      </div>
    `;

    body.appendChild(form);
    body.appendChild(responseSection);

    container.appendChild(header);
    container.appendChild(body);

    // Initial URL preview after form is added to DOM
    updateUrlPreview(form);
  }

  function getFormParams(formEl) {
    const params = {};
    const form = formEl || document.querySelector(".endpoint-form");
    if (!form) return params;
    
    const inputs = form.querySelectorAll("input[name], textarea");

    inputs.forEach((input) => {
      const name = input.name;
      const type = input.dataset.type;

      if (type === "boolean") {
        params[name] = input.checked;
      } else if (type === "textarea") {
        params[name] = input.value;
      } else {
        if (input.value.trim()) {
          params[name] = input.value.trim();
        }
      }
    });

    return params;
  }

  async function executeRequest(formEl) {
    if (!currentEndpoint) return;

    const ep = currentEndpoint;
    const params = getFormParams(formEl);
    const viewer = document.getElementById("responseViewer");
    const statusEl = document.getElementById("responseStatus");

    viewer.innerHTML = '<div class="empty-response">Sending request...</div>';
    statusEl.innerHTML = "";

    try {
      let url, options;

      if (ep.method === "GET") {
        url = apiURL(ep.path, params);
        options = { method: "GET", cache: "no-store" };
      } else {
        // POST
        url = apiURL(ep.path, {});
        let body = "";
        let contentType = "application/json"; // Default for POST

        if (ep.id === "rollup_backfill") {
          body = params.body || "{}";
        } else if (ep.id === "import") {
          body = params.body || "";
          contentType = "text/plain"; // Line Protocol expects text/plain
        } else if (ep.id === "events_create") {
          // For events, parse the body JSON and inject db parameter
          try {
            const bodyJSON = JSON.parse(params.body || "[]");
            const db = params.db;
            let events = Array.isArray(bodyJSON) ? bodyJSON : [bodyJSON];
            events = events.map(event => ({ ...event, db }));
            body = JSON.stringify(events);
          } catch (e) {
            body = params.body || "[]";
          }
        } else if (ep.id === "internal_events_groups_set") {
          body = params.body || "{}";
        } else {
          body = JSON.stringify(params);
        }

        options = {
          method: "POST",
          cache: "no-store",
          body: body,
          headers: { "Content-Type": contentType },
        };
      }

      const response = await fetch(url, options);
      const text = await response.text();

      let data;
      try {
        data = JSON.parse(text);
      } catch {
        data = text;
      }

      // Display status
      const statusClass = response.ok ? "success" : "error";
      statusEl.innerHTML = `<div class="response-status ${statusClass}">HTTP ${response.status}</div>`;

      // Display response
      if (typeof data === "string") {
        viewer.innerHTML = `<pre>${escapeHtml(data)}</pre>`;
      } else {
        const jsonStr = JSON.stringify(data, null, 2);
        const colorized = colorizeJson(jsonStr);
        viewer.innerHTML = `<pre><code class="language-json">${colorized}</code></pre>`;
      }
    } catch (error) {
      statusEl.innerHTML = `<div class="response-status error">Error</div>`;
      viewer.innerHTML = `<pre style="color: #f44336;">${escapeHtml(error.message)}</pre>`;
    }
  }

  function updateUrlPreview(formEl) {
    if (!currentEndpoint) return;

    const ep = currentEndpoint;
    const params = getFormParams(formEl);
    let url;
    
    if (ep.method === "GET") {
      url = apiURL(ep.path, params);
    } else {
      // For POST, just show the path (parameters go in the body)
      url = apiURL(ep.path, {});
    }
    
    const urlInput = document.getElementById("urlPreviewInput");
    if (urlInput) {
      urlInput.value = url;
    }
  }

  function escapeHtml(text) {
    const div = document.createElement("div");
    div.textContent = text;
    return div.innerHTML;
  }

  function colorizeJson(jsonStr) {
    const escaped = escapeHtml(jsonStr);
    return escaped.replace(/("(\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?)/g, (match) => {
      let cls = 'json-number';
      if (/^"/.test(match)) {
        cls = match.endsWith(':') ? 'json-key' : 'json-string';
      } else if (/true|false/.test(match)) {
        cls = 'json-boolean';
      } else if (/null/.test(match)) {
        cls = 'json-null';
      }
      return `<span class="${cls}">${match}</span>`;
    });
  }

  function filterEndpoints() {
    const query = document.getElementById("searchInput").value.toLowerCase();
    const buttons = document.querySelectorAll(".endpoint-btn");

    buttons.forEach((btn) => {
      const text = btn.textContent.toLowerCase();
      btn.style.display = text.includes(query) ? "block" : "none";
    });
  }

  // Initialize
  document.addEventListener("DOMContentLoaded", () => {
    renderEndpointsList();

    document.getElementById("searchInput").addEventListener("input", filterEndpoints);

    // Auto-select first endpoint
    if (ENDPOINTS.length > 0) {
      const firstBtn = document.querySelector(".endpoint-btn");
      if (firstBtn) firstBtn.click();
    }
  });
})();
