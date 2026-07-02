#!/usr/bin/env node
// Capture a full-page screenshot of a page at a fixed CSS width using headless Chromium and
// the DevTools Protocol (captureBeyondViewport → the whole scrollable page, not just the
// viewport). No dependencies beyond Chromium + Node (uses the built-in fetch/WebSocket).
//
// Usage: node screenshot.mjs [URL] [WIDTH] [OUTPUT]
//   URL     defaults to the 3-image comparison link
//   WIDTH   CSS width in px (default 754)
//   OUTPUT  PNG path (default preview.png in the current directory)
//
// Override the browser binary with CHROME=/path/to/chromium.

import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, delimiter } from "node:path";

const DEFAULT_URL =
  "http://dic.localhost:8080/?image=lucaslorentz%2Fcaddy-docker-proxy%3Aci-alpine&image=alpine%3Alatest&image=ghcr.io%2Freeywhaar%2Fdoapi%3Alatest&platform=linux%2Famd64";

const url = process.argv[2] || DEFAULT_URL;
const width = parseInt(process.argv[3] || "754", 10);
const out = process.argv[4] || "preview.png";

// Locate a Chromium/Chrome binary: CHROME env, then known macOS apps, then PATH.
function findChrome() {
  const candidates = [
    process.env.CHROME,
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  ].filter(Boolean);
  for (const c of candidates) if (existsSync(c)) return c;
  for (const dir of (process.env.PATH || "").split(delimiter)) {
    for (const name of ["chromium", "google-chrome", "google-chrome-stable", "chrome"]) {
      const p = join(dir, name);
      if (existsSync(p)) return p;
    }
  }
  return null;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function main() {
  const chrome = findChrome();
  if (!chrome) {
    console.error("No Chromium/Chrome binary found. Set CHROME=/path/to/chromium.");
    process.exit(1);
  }

  const port = 9000 + Math.floor(Math.random() * 1000);
  const profile = mkdtempSync(join(tmpdir(), "dic-shot-"));
  const child = spawn(
    chrome,
    [
      "--headless=new",
      "--disable-gpu",
      "--hide-scrollbars",
      `--remote-debugging-port=${port}`,
      `--user-data-dir=${profile}`,
      "about:blank",
    ],
    { stdio: "ignore" }
  );

  try {
    // Wait for the DevTools endpoint to come up.
    let ready = false;
    for (let i = 0; i < 100; i++) {
      try {
        const r = await fetch(`http://127.0.0.1:${port}/json/version`);
        if (r.ok) { ready = true; break; }
      } catch {}
      await sleep(100);
    }
    if (!ready) throw new Error("DevTools endpoint did not start");

    await capture(port, url, width, out);
    console.log(`wrote ${out} (width ${width})`);
  } finally {
    child.kill();
    await sleep(300); // let Chromium finish writing its profile before removing it
    rmSync(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
  }
}

async function capture(port, url, width, out) {
  // Grab the first "page" target's debugger websocket.
  const targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json();
  const page = targets.find((t) => t.type === "page");
  if (!page) throw new Error("no page target");

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });

  let nextId = 1;
  const pending = new Map();
  const waiters = [];
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
    } else if (msg.method) {
      for (let i = waiters.length - 1; i >= 0; i--) {
        if (waiters[i].method === msg.method) { waiters[i].resolve(msg.params); waiters.splice(i, 1); }
      }
    }
  };
  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const id = nextId++;
      pending.set(id, { resolve, reject });
      ws.send(JSON.stringify({ id, method, params }));
    });
  const waitEvent = (method, ms = 15000) =>
    new Promise((resolve, reject) => {
      waiters.push({ method, resolve });
      setTimeout(() => reject(new Error(`timeout ${method}`)), ms);
    });

  await send("Page.enable");
  // Fixed CSS width, auto height, 2x for a crisp image.
  await send("Emulation.setDeviceMetricsOverride", { width, height: 800, deviceScaleFactor: 2, mobile: false });

  const loaded = waitEvent("Page.loadEventFired");
  await send("Page.navigate", { url });
  await loaded;
  await sleep(500); // settle fonts/layout

  const { data } = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
  writeFileSync(out, Buffer.from(data, "base64"));
  ws.close();
}

main().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});
