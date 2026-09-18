#!/usr/bin/env node
// Weekly venue report -> PDF.
//
// Runs on a workstation, not on the MDM box. The server container is a bare alpine with
// no browser and a 512 MB cap, and the one time that database ran out of memory it took
// Postgres down with it; a Chromium render spikes several hundred megabytes. A weekly
// job has no business in the request path of the machine it reports on, so it happens
// here and the finished PDF is posted up.
//
// For each venue it opens the report page for the last COMPLETED week, prints it, and
// POSTs the bytes to the MDM, which stores them under an unguessable URL that the
// weekly email and the owner's home page both link to.
//
//   MDM_URL=https://mdm.dev.aioapp.com \
//   MDM_USER=... MDM_PASS=... ADMIN_API_KEY=... \
//   node render.mjs [--week YYYY-MM-DD] [--venue <uuid>] [--keep <dir>]
//
// Exit code is non-zero if any venue failed, so cron mails the operator.

import { chromium } from 'playwright';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

const cfg = {
  url: (process.env.MDM_URL || '').replace(/\/+$/, ''),
  user: process.env.MDM_USER || '',
  pass: process.env.MDM_PASS || '',
  key: process.env.ADMIN_API_KEY || '',
};
for (const [k, v] of Object.entries(cfg)) {
  if (!v) {
    console.error(`missing env: ${{ url: 'MDM_URL', user: 'MDM_USER', pass: 'MDM_PASS', key: 'ADMIN_API_KEY' }[k]}`);
    process.exit(2);
  }
}

const args = process.argv.slice(2);
const argOf = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : null;
};
const onlyVenue = argOf('--venue');
const keepDir = argOf('--keep');

// The Monday..Sunday of the most recently completed week, matching lastFullWeek() on
// the server. Both must agree: the server derives the storage token from this date, so
// a mismatch here silently files the PDF under a week nothing links to.
function lastFullWeek(now = new Date()) {
  const today = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const back = today.getUTCDay() === 0 ? 7 : today.getUTCDay(); // Sunday = 0
  const to = new Date(today); to.setUTCDate(to.getUTCDate() - back);
  const from = new Date(to); from.setUTCDate(from.getUTCDate() - 6);
  return { from, to };
}
const iso = (d) => d.toISOString().slice(0, 10);

const week = argOf('--week') || iso(lastFullWeek().to);
if (!/^\d{4}-\d{2}-\d{2}$/.test(week)) {
  console.error(`--week must be YYYY-MM-DD, got ${week}`);
  process.exit(2);
}

async function listVenues() {
  const res = await fetch(`${cfg.url}/api/v1/restaurants`, { headers: { 'X-API-Key': cfg.key } });
  if (!res.ok) throw new Error(`list venues: HTTP ${res.status}`);
  return res.json();
}

async function upload(id, pdf) {
  const res = await fetch(`${cfg.url}/api/v1/restaurants/${id}/report.pdf?week=${week}`, {
    method: 'POST',
    headers: { 'X-API-Key': cfg.key, 'Content-Type': 'application/pdf' },
    body: pdf,
  });
  if (!res.ok) throw new Error(`upload: HTTP ${res.status} ${(await res.text()).slice(0, 200)}`);
}

const venues = (await listVenues()).filter((v) => !onlyVenue || v.id === onlyVenue);
if (venues.length === 0) {
  console.error('no venues to render');
  process.exit(1);
}
if (keepDir) await mkdir(keepDir, { recursive: true });

const browser = await chromium.launch();
// One context for the whole run: logging in once per venue would be a login storm
// against the rate limiter for no reason.
const ctx = await browser.newContext({ viewport: { width: 1280, height: 1600 } });
const page = await ctx.newPage();

let failed = 0;
try {
  // A real browser navigation, so the login's Origin/CSRF checks see what they expect —
  // this is why the job drives a browser rather than posting the form with fetch.
  await page.goto(`${cfg.url}/login`, { waitUntil: 'domcontentloaded' });
  await page.fill('input[name="username"]', cfg.user);
  await page.fill('input[name="password"]', cfg.pass);
  await Promise.all([
    page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 30000 }),
    page.click('button[type="submit"]'),
  ]);

  for (const v of venues) {
    const label = `${v.name} (${v.id})`;
    try {
      const url = `${cfg.url}/restaurants/${v.id}/report`;
      await page.goto(url, { waitUntil: 'networkidle', timeout: 60000 });

      // Fail loudly rather than filing a login page as a report: a session that expired
      // mid-run would otherwise produce a perfectly valid PDF of the wrong thing.
      if (new URL(page.url()).pathname.startsWith('/login')) {
        throw new Error('redirected to login — session lost');
      }
      if (!(await page.locator('.cc-kpis').count())) {
        throw new Error('report body did not render (no KPI row)');
      }

      // print:false so the PDF matches the page as designed rather than any print
      // stylesheet; backgrounds on, because the report's meaning is partly in colour.
      const pdf = await page.pdf({
        format: 'A4',
        printBackground: true,
        margin: { top: '12mm', bottom: '12mm', left: '10mm', right: '10mm' },
      });

      if (keepDir) await writeFile(join(keepDir, `${v.id}-${week}.pdf`), pdf);
      await upload(v.id, pdf);
      console.log(`ok   ${label} — ${(pdf.length / 1024).toFixed(0)} kB`);
    } catch (err) {
      failed++;
      console.error(`FAIL ${label}: ${err.message}`);
    }
  }
} finally {
  await browser.close();
}

console.log(`week ending ${week}: ${venues.length - failed}/${venues.length} rendered`);
process.exit(failed ? 1 : 0);
