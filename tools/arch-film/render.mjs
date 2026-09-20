#!/usr/bin/env node
// Renders film.html to an MP4, one deterministic frame at a time.
//
// The film is a pure function of t: nothing animates on a wall clock, and every frame
// is produced by calling window.seek(t) and screenshotting. So the output does not
// depend on how fast this machine is or how long a screenshot took — re-running gives
// byte-comparable frames. That is worth more than speed for something meant to be
// re-rendered whenever the architecture changes.
//
// RUN
//   cd mdm-server/tools/arch-film && npm install && node render.mjs
//   node render.mjs --fps 30 --scale 1 --out ../../../mdm-architecture.mp4
//   node render.mjs --from 34 --to 50        # just one scene, while editing it
//
// Needs ffmpeg on PATH (system ffmpeg is fine here — no xfade, just a frame sequence).

import { chromium } from 'playwright';
import { mkdir, rm, readdir } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const argOf = (n, d) => { const i = args.indexOf(n); return i >= 0 ? args[i + 1] : d; };

const FPS    = Number(argOf('--fps', 30));
const SCALE  = Number(argOf('--scale', 1));          // 1 = 1920x1080, 0.5 = 960x540
const OUT    = resolve(here, argOf('--out', 'out/mdm-architecture.mp4'));
const FROM   = argOf('--from', null) === null ? null : Number(argOf('--from'));
const TO     = argOf('--to', null) === null ? null : Number(argOf('--to'));
const FRAMES = join(here, 'frames');

const W = Math.round(1920 * SCALE), H = Math.round(1080 * SCALE);

await rm(FRAMES, { recursive: true, force: true });
await mkdir(FRAMES, { recursive: true });
await mkdir(dirname(OUT), { recursive: true });

const browser = await chromium.launch();
const page = await browser.newPage({
  viewport: { width: W, height: H },
  deviceScaleFactor: 1,
});
await page.goto('file://' + join(here, 'film.html'), { waitUntil: 'load' });
// Webfonts must be in before the first frame or the opening seconds render in a
// fallback face and the type jumps once they arrive.
await page.evaluate(() => document.fonts.ready);
if (SCALE !== 1) {
  await page.addStyleTag({ content: `#stage{transform:scale(${SCALE});transform-origin:0 0}` });
}

const duration = await page.evaluate(() => window.FILM_DURATION);
const t0 = FROM ?? 0, t1 = TO ?? duration;
const total = Math.ceil((t1 - t0) * FPS);
console.log(`film ${duration.toFixed(1)}s · rendering ${t0}–${t1.toFixed(1)}s · ${total} frames at ${W}x${H}`);

const started = Date.now();
for (let f = 0; f < total; f++) {
  const t = t0 + f / FPS;
  await page.evaluate((x) => window.seek(x), t);
  await page.screenshot({
    path: join(FRAMES, String(f).padStart(5, '0') + '.png'),
    animations: 'disabled',
  });
  if (f % 60 === 0 || f === total - 1) {
    const pct = ((f + 1) / total * 100).toFixed(0);
    const rate = (f + 1) / ((Date.now() - started) / 1000);
    process.stdout.write(`\r  ${pct}%  ${f + 1}/${total}  ${rate.toFixed(1)} fps`);
  }
}
process.stdout.write('\n');
await browser.close();

console.log('encoding…');
await new Promise((ok, fail) => {
  const ff = spawn('ffmpeg', [
    '-y', '-framerate', String(FPS),
    '-i', join(FRAMES, '%05d.png'),
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '18',
    // yuv420p so it plays in browsers and Quicktime, not just in mpv.
    '-pix_fmt', 'yuv420p', '-movflags', '+faststart',
    OUT,
  ], { stdio: ['ignore', 'ignore', 'pipe'] });
  let err = '';
  ff.stderr.on('data', d => { err += d; });
  ff.on('close', c => c === 0 ? ok() : fail(new Error(err.slice(-1500))));
});

const frames = (await readdir(FRAMES)).length;
console.log(`done — ${frames} frames → ${OUT}`);
