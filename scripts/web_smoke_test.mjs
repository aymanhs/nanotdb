import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { setTimeout as delay } from "node:timers/promises";
import { spawn } from "node:child_process";
import net from "node:net";
import { chromium } from "playwright";

function log(message) {
  process.stdout.write(`${message}\n`);
}

async function reservePort() {
  const server = net.createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (!address || typeof address === "string") {
    server.close();
    throw new Error("failed to reserve TCP port for web smoke test");
  }
  const { port } = address;
  await new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
  return port;
}

async function writeFixture(rootDir, port) {
  const dashboard = {
    title: "Browser Smoke",
    default_db: "metrics",
    groups: [
      {
        id: "overview",
        label: "Overview",
        widgets: ["error_card", "dup_chart"],
      },
    ],
    widgets: {
      error_card: {
        type: "number",
        title: "Broken Query",
        refresh_sec: 10,
        series: [{ metric: "test.metric" }],
      },
      dup_chart: {
        type: "line_chart",
        title: "Duplicate Labels",
        refresh_sec: 10,
        lookback: "1h",
        interval: "1m",
        series: [
          { label: "CPU", metric: "temp.cpu" },
          { label: "CPU", metric: "temp.gpu" },
        ],
      },
    },
  };
  const engineToml = `[engine]\nlisten = \":${port}\"\n\n[wal]\nflush_interval_ms = 1000\nmax_segment_size = 1048576\n\n[web]\nenabled = true\nbase_path = \"/dashboard\"\nexplore_path = \"/explore\"\nengine_path = \"/engine\"\ntitle = \"Browser Smoke\"\ndashboard_config = \"dashboard.json\"\n`;
  await writeFile(path.join(rootDir, "dashboard.json"), JSON.stringify(dashboard, null, 2) + "\n");
  await writeFile(path.join(rootDir, "engine.toml"), engineToml);
}

async function waitForServer(url, attempts = 60) {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    try {
      const res = await fetch(url, { cache: "no-store" });
      if (res.ok) {
        return;
      }
    } catch {
      // keep polling while the server boots
    }
    await delay(500);
  }
  throw new Error(`server did not become ready at ${url}`);
}

function startServer(configPath, cwd) {
  const child = spawn("go", ["run", "./cmd/nanotdb", "--config", configPath], {
    cwd,
    env: process.env,
    stdio: ["ignore", "pipe", "pipe"],
  });

  child.stdout.on("data", (chunk) => process.stdout.write(chunk));
  child.stderr.on("data", (chunk) => process.stderr.write(chunk));

  return child;
}

function uPlotStubJS() {
  return `
    window.__uPlotSeriesLabels = [];
    window.__uPlotData = null;
    window.uPlot = function(opts, data, el) {
      window.__uPlotSeriesLabels = (opts.series || []).slice(1).map(function(series) { return series.label; });
      window.__uPlotData = data;
      const legend = document.createElement('div');
      legend.className = 'u-legend';
      window.__uPlotSeriesLabels.forEach(function(label) {
        const span = document.createElement('span');
        span.className = 'u-label';
        span.textContent = label;
        legend.appendChild(span);
      });
      el.innerHTML = '';
      el.appendChild(legend);
      return {
        destroy() { el.innerHTML = ''; },
        setSize() {},
        setData(nextData) { window.__uPlotData = nextData; }
      };
    };
  `;
}

async function run() {
  const repoRoot = process.cwd();
  const tempRoot = await mkdtemp(path.join(os.tmpdir(), "nanotdb-web-smoke-"));
  const configPath = path.join(tempRoot, "engine.toml");
  let server;
  let browser;

  try {
    const port = await reservePort();
    const baseURL = `http://127.0.0.1:${port}`;
    const queryRangeWindows = [];
    await writeFixture(tempRoot, port);
    server = startServer(configPath, repoRoot);
    await waitForServer(`${baseURL}/api/dashboard-config`);

    try {
      browser = await chromium.launch({ headless: true });
    } catch (err) {
      const message = err && err.message ? String(err.message) : String(err);
      if (message.includes("Executable doesn't exist")) {
        throw new Error(`${message}\nRun: npx playwright install`);
      }
      throw err;
    }
    const context = await browser.newContext();

    await context.route("https://unpkg.com/uplot@**/*.css", async (route) => {
      await route.fulfill({ status: 200, contentType: "text/css", body: "" });
    });
    await context.route("https://unpkg.com/uplot@**/*.js", async (route) => {
      await route.fulfill({ status: 200, contentType: "application/javascript", body: uPlotStubJS() });
    });
    await context.route(`${baseURL}/api/v1/query?**`, async (route) => {
      await route.fulfill({ status: 500, contentType: "text/plain", body: "forced query failure" });
    });
    await context.route(`${baseURL}/api/v1/query_range?**`, async (route) => {
      const url = new URL(route.request().url());
      const query = url.searchParams.get("query") || "";
      const start = Date.parse(url.searchParams.get("start") || "");
      const end = Date.parse(url.searchParams.get("end") || "");
      if (Number.isFinite(start) && Number.isFinite(end) && end > start) {
        queryRangeWindows.push(Math.round((end - start) / 1000));
      }
      const values = query === "temp.cpu"
        ? [[1710000000, "1"], [1710000060, "2"]]
        : [[1710000000, "3"], [1710000060, "4"]];
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ data: { result: [{ values }] } }),
      });
    });

    const dashboardPage = await context.newPage();
    await dashboardPage.goto(`${baseURL}/dashboard`, { waitUntil: "domcontentloaded" });
    await dashboardPage.waitForFunction(() => {
      const foot = document.querySelector(".widget-number .widget-foot");
      const labels = window.__uPlotSeriesLabels;
      const lookback = document.querySelector(".widget-lookback-select");
      return Boolean(
        foot &&
        foot.textContent &&
        foot.textContent.includes("refresh failed") &&
        Array.isArray(labels) &&
        labels.length === 2 &&
        lookback
      );
    });

    await dashboardPage.selectOption(".widget-lookback-select", "24h");
    await dashboardPage.waitForFunction(() => {
      const chartFoot = document.querySelector(".widget-chart .widget-foot");
      return chartFoot && chartFoot.textContent && chartFoot.textContent.includes("24h");
    });

    const dashboardResult = await dashboardPage.evaluate(() => ({
      refreshErrorText: document.querySelector(".widget-number .widget-foot")?.textContent || "",
      refreshErrorClass: document.querySelector(".widget-number")?.classList.contains("widget-refresh-error") || false,
      labels: Array.isArray(window.__uPlotSeriesLabels) ? window.__uPlotSeriesLabels.slice() : [],
      dataColumns: Array.isArray(window.__uPlotData) ? window.__uPlotData.length : 0,
      lookbackValue: document.querySelector(".widget-lookback-select")?.value || "",
      chartFoot: document.querySelector(".widget-chart .widget-foot")?.textContent || "",
    }));

    if (!dashboardResult.refreshErrorClass || !dashboardResult.refreshErrorText.includes("refresh failed")) {
      throw new Error(`dashboard did not surface per-widget refresh error: ${JSON.stringify(dashboardResult)}`);
    }
    if (dashboardResult.labels.length !== 2 || dashboardResult.labels[0] !== "CPU" || dashboardResult.labels[1] !== "CPU") {
      throw new Error(`dashboard did not preserve duplicate chart labels: ${JSON.stringify(dashboardResult)}`);
    }
    if (dashboardResult.dataColumns !== 3) {
      throw new Error(`dashboard chart did not build expected data columns: ${JSON.stringify(dashboardResult)}`);
    }
    if (dashboardResult.lookbackValue !== "24h" || !dashboardResult.chartFoot.includes("24h")) {
      throw new Error(`dashboard lookback control did not update chart state: ${JSON.stringify(dashboardResult)}`);
    }
    if (!queryRangeWindows.some((seconds) => Math.abs(seconds - 3600) <= 2)) {
      throw new Error(`dashboard did not issue initial 1h query_range window: ${JSON.stringify(queryRangeWindows)}`);
    }
    if (!queryRangeWindows.some((seconds) => Math.abs(seconds - 86400) <= 2)) {
      throw new Error(`dashboard did not issue updated 24h query_range window: ${JSON.stringify(queryRangeWindows)}`);
    }

    const editorPage = await context.newPage();
    await editorPage.goto(`${baseURL}/dashboard/edit`, { waitUntil: "domcontentloaded" });
    const validateResult = await editorPage.evaluate(async () => {
      const payload = {
        title: "Browser Smoke",
        default_db: "metrics",
        groups: [{ id: "overview", label: "Overview", widgets: ["dup_chart"] }],
        widgets: {
          dup_chart: {
            type: "line_chart",
            title: "Duplicate Labels",
            lookback: "1h",
            interval: "1m",
            series: [
              { label: "CPU", metric: "temp.cpu" },
              { label: "CPU", metric: "temp.gpu" },
            ],
          },
        },
      };
      const res = await fetch("/api/dashboard-config/validate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });
      return { status: res.status, body: await res.json() };
    });

    if (validateResult.status !== 400) {
      throw new Error(`editor validate endpoint returned unexpected status: ${JSON.stringify(validateResult)}`);
    }
    if (!Array.isArray(validateResult.body.errors) || !validateResult.body.errors.some((msg) => msg.includes("duplicate line chart label"))) {
      throw new Error(`editor validate endpoint did not reject duplicate labels: ${JSON.stringify(validateResult)}`);
    }

    log("web smoke test passed");
  } finally {
    if (browser) {
      await browser.close();
    }
    if (server && !server.killed) {
      server.kill("SIGTERM");
      await Promise.race([
        new Promise((resolve) => server.once("exit", resolve)),
        delay(5000).then(() => {
          if (!server.killed) {
            server.kill("SIGKILL");
          }
        }),
      ]);
    }
    await rm(tempRoot, { recursive: true, force: true });
  }
}

run().catch((err) => {
  console.error(err && err.stack ? err.stack : String(err));
  process.exitCode = 1;
});