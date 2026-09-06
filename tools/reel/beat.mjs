#!/usr/bin/env node
// Estimate a track's tempo and print BPM + beat length, so assemble.sh can snap
// clip boundaries to the music.  node beat.mjs track.mp3  → {"bpm":126,"beat":0.476}
// Method: onset-energy envelope (10 ms hops) → autocorrelation over 70–170 BPM.

import { execFileSync } from "node:child_process";

const file = process.argv[2];
if (!file) { console.error("usage: node beat.mjs track.mp3"); process.exit(1); }
const SR = 11025, HOP = 110; // ~10 ms
const pcm = execFileSync("ffmpeg", ["-v", "error", "-i", file, "-t", "90", "-ac", "1", "-ar", String(SR), "-f", "s16le", "-"], { maxBuffer: 1 << 28 });
const n = Math.floor(pcm.length / 2);
const frames = Math.floor(n / HOP);
const env = new Float64Array(frames);
for (let f = 0; f < frames; f++) {
  let e = 0;
  for (let i = f * HOP; i < (f + 1) * HOP; i++) { const v = pcm.readInt16LE(i * 2) / 32768; e += v * v; }
  env[f] = Math.sqrt(e / HOP);
}
// onset strength = positive change in envelope
const on = new Float64Array(frames);
for (let f = 1; f < frames; f++) on[f] = Math.max(0, env[f] - env[f - 1]);
const mean = on.reduce((a, b) => a + b, 0) / frames;
for (let f = 0; f < frames; f++) on[f] -= mean;
const hopSec = HOP / SR;
let best = { bpm: 0, score: -Infinity };
for (let bpm = 70; bpm <= 170; bpm += 0.5) {
  const lag = Math.round(60 / bpm / hopSec);
  let s = 0;
  for (let f = 0; f + lag < frames; f++) s += on[f] * on[f + lag];
  // also reward the double-lag (bar-level periodicity) to disambiguate half/double time
  const lag2 = lag * 2; let s2 = 0;
  for (let f = 0; f + lag2 < frames; f++) s2 += on[f] * on[f + lag2];
  const score = s + 0.5 * s2;
  if (score > best.score) best = { bpm, score };
}
console.log(JSON.stringify({ bpm: best.bpm, beat: +(60 / best.bpm).toFixed(4) }));
