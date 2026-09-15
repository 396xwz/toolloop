#!/usr/bin/env node
// scripts/scrape.mjs — Node + Playwright implementation of the "scrapling"
// CLI contract spoken by toolloop's scrape tool (internal/tools/tools.go):
//
//   node scripts/scrape.mjs extract <mode> <url> <output> [flags]
//
// Modes:
//   get             Plain HTTP GET, no browser. --timeout is in seconds.
//   fetch           Headless Chromium renders the page. --timeout/--wait in ms.
//   stealthy-fetch  Same as fetch, with the anti-automation tells stripped.
//
// Flags:
//   --css-selector <sel>  Extract innerText of the matching elements
//   --timeout <n>         get: seconds | fetch/stealthy-fetch: milliseconds
//   --wait <ms>           Extra settle delay after load (fetch modes only)
//   --network-idle        Soft wait for network idle (fetch modes only)
//   --block-ads           Block ad/tracking hosts (fetch modes only)
//   --ai-targeted         Clean plain-text output (fetch modes only)
//
// The full extracted content is written to <output>; a short status line is
// printed to stdout. Exit code 0 on success, 1 on failure.
//
// Setup:  npm install && npx playwright install chromium

import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import process from "node:process";

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

// Real Chrome UAs per platform. Headless Chromium's default
// "HeadlessChrome/..." UA is an instant bot tell, so we always override it.
// In the fetch modes the Chrome version is taken from the actually launched
// browser (browser.version()): CDNs such as Akamai cross-check the UA against
// the TLS/HTTP2 fingerprint of the real binary, so a stale hardcoded version
// gets 403'd.
const BASE_UA_BY_PLATFORM = {
  win32:
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) ",
  darwin:
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) ",
  linux:
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) ",
};
// No browser fingerprint to match in the plain-GET mode, so a current static
// version is fine there.
const STATIC_CHROME_VERSION = "153.0.8010.12";
const CH_PLATFORM_BY_OS = { win32: "Windows", darwin: "macOS", linux: "Linux" };

function userAgentFor(version) {
  return (
    (BASE_UA_BY_PLATFORM[process.platform] ?? BASE_UA_BY_PLATFORM.linux) +
    `Chrome/${version} Safari/537.36`
  );
}
const USER_AGENT = userAgentFor(STATIC_CHROME_VERSION);

// Hosts (and subdomains) aborted when --block-ads is set. Ads and telemetry,
// never page-critical resources.
const AD_HOSTS = [
  "doubleclick.net",
  "googlesyndication.com",
  "googleadservices.com",
  "googletagmanager.com",
  "googletagservices.com",
  "adsafeprotected.com",
  "adnxs.com",
  "adform.net",
  "adcolony.com",
  "amazon-adsystem.com",
  "applovin.com",
  "criteo.com",
  "criteo.net",
  "adroll.com",
  "pubmatic.com",
  "rubiconproject.com",
  "openx.net",
  "indexww.com",
  "smartadserver.com",
  "taboola.com",
  "outbrain.com",
  "moatads.com",
  "scorecardresearch.com",
  "quantserve.com",
  "hotjar.com",
  "mixpanel.com",
  "segment.com",
  "segment.io",
  "amplitude.com",
  "fullstory.com",
  "newrelic.com",
  "nr-data.net",
  "sentry.io",
  "cloudflareinsights.com",
  "facebook.net",
  "ads-twitter.com",
];

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

function die(msg, code = 1) {
  console.error(`scrape.mjs: ${msg}`);
  process.exit(code);
}

function isHeadless() {
  const v = process.env.SCRAPE_HEADFUL ?? "";
  return !(v === "1" || v.toLowerCase() === "true");
}

async function loadPlaywright() {
  try {
    return await import("playwright");
  } catch {
    die(
      "playwright is not installed; run: npm install && npx playwright install chromium"
    );
  }
}

const NAMED_ENTITIES = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
  copy: "©",
  reg: "®",
  trade: "™",
  mdash: "—",
  ndash: "–",
  hellip: "…",
  laquo: "«",
  raquo: "»",
  lsquo: "‘",
  rsquo: "’",
  ldquo: "“",
  rdquo: "”",
  times: "×",
  divide: "÷",
};

function safeChar(code) {
  try {
    return String.fromCodePoint(code);
  } catch {
    return "";
  }
}

function decodeEntities(s) {
  return s
    .replace(/&#x([0-9a-fA-F]+);/g, (_, h) => safeChar(parseInt(h, 16)))
    .replace(/&#(\d+);/g, (_, d) => safeChar(parseInt(d, 10)))
    .replace(
      /&([a-zA-Z]+);/g,
      (m, name) => NAMED_ENTITIES[name.toLowerCase()] ?? m
    );
}

// Dependency-free HTML -> plain text for the no-browser `get` mode.
function htmlToText(html) {
  let s = html;
  s = s.replace(/<script[\s\S]*?<\/script\s*>/gi, " ");
  s = s.replace(/<style[\s\S]*?<\/style\s*>/gi, " ");
  s = s.replace(/<noscript[\s\S]*?<\/noscript\s*>/gi, " ");
  s = s.replace(/<template[\s\S]*?<\/template\s*>/gi, " ");
  s = s.replace(/<svg[\s\S]*?<\/svg\s*>/gi, " ");
  s = s.replace(/<!--[\s\S]*?-->/g, " ");
  s = s.replace(/<br\s*\/?>/gi, "\n");
  s = s.replace(
    /<\/(p|div|section|article|li|tr|h[1-6]|header|footer|nav|table|blockquote|pre|main|aside|ul|ol|dl|form|fieldset|details|figure|figcaption|hr)\s*>/gi,
    "\n"
  );
  s = s.replace(/<li[^>]*>/gi, "- ");
  s = s.replace(/<[^>]+>/g, " ");
  s = decodeEntities(s);
  s = s.replace(/[ \t]+/g, " ");
  s = s.replace(/ ?\n ?/g, "\n");
  s = s.replace(/\n{3,}/g, "\n\n");
  return s.trim();
}

function titleOf(html) {
  const m = html.match(/<title[^>]*>([\s\S]*?)<\/title>/i);
  return m ? decodeEntities(m[1]).replace(/\s+/g, " ").trim() : "";
}

function normalize(s) {
  return s.replace(/[ \t]+\n/g, "\n").replace(/\n{3,}/g, "\n\n").trim();
}

async function extractText(page, selector) {
  if (selector) {
    const parts = await page
      .locator(selector)
      .allInnerTexts()
      .catch(() => []);
    return parts
      .map((p) => p.trim())
      .filter(Boolean)
      .join("\n\n");
  }
  try {
    return await page.evaluate(() =>
      document.body ? document.body.innerText : ""
    );
  } catch {
    return "";
  }
}

function finish(output, finalUrl, title, text, statusLine) {
  const body = normalize(text);
  const header = title ? `# ${title}\n\n` : "";
  const content = header + body + "\n";
  const dir = dirname(output);
  if (dir && dir !== ".") mkdirSync(dir, { recursive: true });
  writeFileSync(output, content, "utf8");
  console.log(
    `scraped ${finalUrl} [${statusLine}] -> ${output} (${content.length} chars)`
  );
}

function launchOptions() {
  return {
    headless: isHeadless(),
    // Strips the blink feature that surfaces in the "Chrome is being
    // controlled by automated software" infobar and related tells.
    args: ["--disable-blink-features=AutomationControlled"],
  };
}

// ---------------------------------------------------------------------------
// Modes
// ---------------------------------------------------------------------------

async function runGet(url, output, flags) {
  const timeoutMs = (Number(flags.timeout) > 0 ? Number(flags.timeout) : 45) * 1000;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(new Error("timeout")), timeoutMs);
  let resp;
  try {
    resp = await fetch(url, {
      redirect: "follow",
      signal: controller.signal,
      headers: {
        "user-agent": USER_AGENT,
        accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        "accept-language": "en-US,en;q=0.9",
      },
    });
  } catch (e) {
    die(`GET failed: ${e.cause?.message ?? e.message}`);
  } finally {
    clearTimeout(timer);
  }

  const html = (await resp.text()).slice(0, 5_000_000);
  if (resp.status >= 400) die(`GET ${url} -> HTTP ${resp.status}`);

  let text;
  if (flags.selector) {
    // CSS selection needs a DOM. Render the already-fetched HTML on a blank
    // page — real CSS matching without a second network round-trip.
    const { chromium } = await loadPlaywright();
    const browser = await chromium.launch(launchOptions());
    try {
      const page = await browser.newPage();
      await page.setContent(html, { waitUntil: "domcontentloaded" });
      text = await extractText(page, flags.selector);
    } finally {
      await browser.close().catch(() => {});
    }
  } else {
    text = htmlToText(html);
  }
  finish(output, resp.url || url, titleOf(html), text, `mode=get status=${resp.status}`);
}

async function runFetch(mode, url, output, flags, stealth) {
  const { chromium } = await loadPlaywright();
  const timeoutMs = Number(flags.timeout) > 0 ? Number(flags.timeout) : 60000;
  const waitMs = Number(flags.wait) > 0 ? Number(flags.wait) : 3000;

  const browser = await chromium.launch(launchOptions());
  try {
    // Match the UA to the launched binary and send the client-hint headers
    // real Chrome sends (the headless shell omits them; Akamai and similar
    // edge WAFs block on the missing hints).
    const version = await browser.version();
    const context = await browser.newContext({
      userAgent: userAgentFor(version),
      locale: "en-US",
      timezoneId: "America/New_York",
      viewport: { width: 1366, height: 768 },
      extraHTTPHeaders: {
        "sec-ch-ua": `"Chromium";v="${version.split(".")[0]}"`,
        "sec-ch-ua-mobile": "?0",
        "sec-ch-ua-platform": `"${CH_PLATFORM_BY_OS[process.platform] ?? "Linux"}"`,
        "upgrade-insecure-requests": "1",
      },
    });

    if (flags.blockAds) {
      await context.route("**/*", (route) => {
        let host = "";
        try {
          host = new URL(route.request().url()).hostname;
        } catch {
          return route.abort("blockedbyclient");
        }
        if (AD_HOSTS.some((h) => host === h || host.endsWith("." + h))) {
          return route.abort("blockedbyclient");
        }
        return route.continue();
      });
    }

    const page = await context.newPage();
    if (stealth) {
      await page.addInitScript(() => {
        // Standard anti-automation-tell patches.
        Object.defineProperty(navigator, "webdriver", { get: () => undefined });
        Object.defineProperty(navigator, "plugins", { get: () => [1, 2, 3, 4, 5] });
        Object.defineProperty(navigator, "languages", {
          get: () => ["en-US", "en"],
        });
        if (!window.chrome) window.chrome = { runtime: {} };
      });
    }

    let resp;
    try {
      resp = await page.goto(url, {
        waitUntil: "domcontentloaded",
        timeout: timeoutMs,
      });
    } catch (e) {
      die(`navigation failed: ${e.message}`);
    }
    const status = resp ? resp.status() : 0;

    if (flags.networkIdle) {
      // Live-odds pages poll forever and never reach networkidle, so this is
      // a soft wait: if it times out, carry on with whatever has rendered.
      try {
        await page.waitForLoadState("networkidle", {
          timeout: Math.min(10000, timeoutMs),
        });
      } catch {
        /* page never went idle — expected for polling sites */
      }
    }
    if (waitMs > 0) await page.waitForTimeout(waitMs);

    if (status >= 400) die(`page returned HTTP ${status} (missing or bot-blocked)`);

    const finalUrl = page.url();
    const title = (await page.title().catch(() => "")).trim();
    const text = await extractText(page, flags.selector);

    if (text.replace(/\s/g, "").length < 20) {
      finish(
        output,
        finalUrl,
        title,
        `[scrape.mjs] Page loaded (HTTP ${status}) but produced little readable text.\n` +
          `Likely bot-blocked, JS-gated, or empty. Try mode=stealthy-fetch, a\n` +
          `--css-selector, or a different URL.`,
        `mode=${mode} status=${status}`
      );
      return;
    }
    finish(output, finalUrl, title, text, `mode=${mode} status=${status}`);
  } finally {
    await browser.close().catch(() => {});
  }
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function usage() {
  console.log(`Usage:
  node scripts/scrape.mjs extract <get|fetch|stealthy-fetch> <url> <output> [flags]

Flags:
  --css-selector <sel>  Extract innerText of the matching elements
  --timeout <n>         get: seconds | fetch/stealthy-fetch: milliseconds
  --wait <ms>           Extra settle delay after load (fetch modes)
  --network-idle        Soft wait for network idle (fetch modes)
  --block-ads           Block ad/tracking hosts (fetch modes)
  --ai-targeted         Clean plain-text output (fetch modes)

Requires Node 18+. Fetch modes additionally need:
  npm install && npx playwright install chromium

Env:
  SCRAPE_HEADFUL=1  Run the browser with a visible window (debugging).`);
}

async function main() {
  const argv = process.argv.slice(2);
  const pos = [];
  const flags = {
    selector: "",
    timeout: "",
    wait: "",
    networkIdle: false,
    blockAds: false,
    aiTargeted: false, // accepted for contract compatibility; normalization is always applied
  };

  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--css-selector") flags.selector = argv[++i] ?? "";
    else if (a === "--timeout") flags.timeout = argv[++i] ?? "";
    else if (a === "--wait") flags.wait = argv[++i] ?? "";
    else if (a === "--network-idle") flags.networkIdle = true;
    else if (a === "--block-ads") flags.blockAds = true;
    else if (a === "--ai-targeted") flags.aiTargeted = true;
    else if (a === "--help" || a === "-h") {
      usage();
      process.exit(0);
    } else if (a.startsWith("--")) {
      die(`unknown flag: ${a}`);
    } else {
      pos.push(a);
    }
  }

  if (pos.length < 4) {
    usage();
    process.exit(1);
  }
  const [cmd, mode, url, output] = pos;
  if (cmd !== "extract") die(`first positional argument must be "extract", got "${cmd}"`);
  if (mode !== "get" && mode !== "fetch" && mode !== "stealthy-fetch") {
    die(`unknown mode "${mode}" (supported: get, fetch, stealthy-fetch)`);
  }

  if (mode === "get") {
    await runGet(url, output, flags);
  } else {
    await runFetch(mode, url, output, flags, mode === "stealthy-fetch");
  }
}

main().catch((e) => die(e?.message ?? String(e)));
