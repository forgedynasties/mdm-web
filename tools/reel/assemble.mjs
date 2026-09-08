#!/usr/bin/env node
// Cut the reel to music: cards + scenes → out/<name>.mp4 (1080p30 H.264 + AAC).
//
//   node assemble.mjs                                   # out/reel.mp4, music/Inspired.mp3
//   MUSIC=music/Wallpaper.mp3 OUT=reel_chill.mp4 node assemble.mjs
//   SCENES="01 02 05 06" SPEED=1.5 OUT=teaser.mp4 node assemble.mjs
//   SCENES_DIR=out/scenes_light OUT=reel_light.mp4 node assemble.mjs
//
// What it does
//   • detects the track's tempo (beat.mjs) and snaps every cut to a half-bar,
//     so scene changes land on the beat
//   • crossfades between clips (XFADE seconds) instead of dipping to black
//   • adds a slow Ken Burns drift on scene clips so nothing sits still
//   • music: fade in, ducked slightly, fades out over the outro; -shortest
//   • drops the blank pre-navigation frames using the .json trim markers

import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync, existsSync } from "node:fs";
import { createRequire } from "node:module";

// System ffmpeg (4.2) predates xfade; use the static build from npm unless FFMPEG is set.
let FFMPEG = process.env.FFMPEG || "ffmpeg";
if (!process.env.FFMPEG) { try { FFMPEG = createRequire(import.meta.url)("ffmpeg-static"); } catch {} }

const env = (k, d) => process.env[k] ?? d;
const OUT = env("OUT", "reel.mp4");
const SCENES_DIR = env("SCENES_DIR", "out/scenes");
const CARDS_DIR = env("CARDS_DIR", "out/cards");
const MUSIC = env("MUSIC", "music/Inspired.mp3");
const ONLY = env("SCENES", "").split(/\s+/).filter(Boolean);
const SPEED = parseFloat(env("SPEED", "1"));
const XFADE = parseFloat(env("XFADE", "0.8"));
const CARD_SEC = parseFloat(env("CARD_SEC", "2.6"));
const CARD_LEAD = 0.6; // cards.mjs keeps the page hidden this long while fonts load
const KB = parseFloat(env("KENBURNS", "0.035")); // total drift (3.5 %)
const W = 1920, H = 1080, FPS = 30;

const run = (args, opts = {}) => execFileSync(args[0], args.slice(1), { stdio: ["ignore", "pipe", "inherit"], maxBuffer: 1 << 26, ...opts }).toString();
const probe = (f) => parseFloat(run(["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", f])) || 0;
const dur = (f) => { const d = probe(f); return d > 0 ? d : (parseFloat(run(["ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", f])) / 25); };

// ---------------------------------------------------------------- tempo
let beat = 0.5;
if (existsSync(MUSIC)) {
  try { beat = JSON.parse(run(["node", new URL("./beat.mjs", import.meta.url).pathname, MUSIC])).beat; } catch {}
}
const HALF_BAR = beat * 2;
const snap = (sec, min) => Math.max(min, Math.floor(sec / HALF_BAR) * HALF_BAR);

// ---------------------------------------------------------------- clip list
const clips = []; // {file, ss, len, card}
const card = (name) => {
  const webm = `${CARDS_DIR}/${name}.webm`, png = `${CARDS_DIR}/${name}.png`;
  // card clips carry their own length (dense cards are rendered longer); snap it
  if (existsSync(webm)) clips.push({ file: webm, ss: CARD_LEAD, len: snap(Math.max(CARD_SEC, dur(webm) - CARD_LEAD - 0.2) + XFADE, HALF_BAR * 2), card: true });
  else if (existsSync(png)) clips.push({ file: png, ss: 0, len: snap(CARD_SEC + XFADE, HALF_BAR * 2), card: true, still: true });
};
card("00-intro");
for (const f of readdirSync(SCENES_DIR).filter((x) => x.endsWith(".webm")).sort()) {
  const idx = f.slice(0, 2), name = f.replace(/\.webm$/, "");
  if (ONLY.length && !ONLY.includes(idx)) continue;
  const meta = existsSync(`${SCENES_DIR}/${name}.json`) ? JSON.parse(readFileSync(`${SCENES_DIR}/${name}.json`, "utf8")) : {};
  card(name);
  // Sidecar times are ms since context creation, which is also the video's t=0
  // (verified: video duration == wall time). Make the camera keyframes clip-relative.
  const ss = Math.max(0, (meta.trimMs || 0) / 1000);
  const raw = (dur(`${SCENES_DIR}/${f}`) - ss) / SPEED;
  const cam = (meta.cam || []).map((e) => ({ ...e, t: e.t - ss * 1000 })).filter((e) => e.t >= 0);
  clips.push({ file: `${SCENES_DIR}/${f}`, ss, len: snap(raw + XFADE, HALF_BAR * 4), scene: true, raw, cam });
}
card("99-outro");

// Build ffmpeg expressions for a clip's camera: z(t) piecewise smoothstep, focus
// point piecewise-constant (only matters while z>1). Times are clip-relative seconds.
function cameraExpr(c) {
  const ev = (c.cam || []).map((e) => ({ t: e.t / 1000 / SPEED, d: (e.dur || 750) / 1000 / SPEED, k: e.k, x: e.x, y: e.y }))
    .filter((e) => e.t < c.len).sort((a, b) => a.t - b.t);
  const kb = KB > 0 ? `(1+${KB}*t/${c.len.toFixed(3)})` : "1";
  if (!ev.length) return { z: kb, px: `${W / 2}`, py: `${H / 2}` };
  // z(t): walk keyframes from the end so nested if()s stay readable
  let z = `${ev[ev.length - 1].k}`;
  let k0 = 1;
  const segs = [];
  for (const e of ev) { segs.push({ t: e.t, d: e.d, from: k0, to: e.k }); k0 = e.k; }
  // rebuild as: if(t<T1, from1, if(t<T1+D1, ease, if(t<T2, to1, ...)))
  const ease = (u) => `(${u})*(${u})*(3-2*(${u}))`;
  let expr = `${segs[segs.length - 1].to}`;
  for (let i = segs.length - 1; i >= 0; i--) {
    const sgm = segs[i];
    const u = `clip((t-${sgm.t.toFixed(3)})/${sgm.d.toFixed(3)},0,1)`;
    const during = `(${sgm.from}+(${sgm.to}-${sgm.from})*${ease(u)})`;
    expr = `if(lt(t,${(sgm.t + sgm.d).toFixed(3)}),${during},${expr})`;
    expr = `if(lt(t,${sgm.t.toFixed(3)}),${sgm.from},${expr})`;
  }
  z = `(${expr})*${kb}`;
  // focus point: pans. Between consecutive zoom-in keyframes the focus glides
  // (smoothstep over the later keyframe's duration) instead of jumping, so a
  // second zoomIn at the same zoom level reads as a camera pan. Zoom-outs keep the
  // last focus so the pull-back is centred where the viewer was looking.
  let px = `${W / 2}`, py = `${H / 2}`;
  const ins = ev.filter((e) => e.k > 1);
  if (ins.length) {
    const last = ins[ins.length - 1];
    px = `${last.x.toFixed(0)}`; py = `${last.y.toFixed(0)}`;
    for (let i = ins.length - 2; i >= 0; i--) {
      const a = ins[i], b = ins[i + 1];
      const u = `clip((t-${b.t.toFixed(3)})/${b.d.toFixed(3)},0,1)`;
      const e = ease(u);
      px = `if(lt(t,${(b.t + b.d).toFixed(3)}),(${a.x.toFixed(0)}+(${(b.x - a.x).toFixed(0)})*${e}),${px})`;
      py = `if(lt(t,${(b.t + b.d).toFixed(3)}),(${a.y.toFixed(0)}+(${(b.y - a.y).toFixed(0)})*${e}),${py})`;
    }
  }
  return { z, px, py };
}

// ---------------------------------------------------------------- filter graph
const inputs = [], filters = [];
clips.forEach((c, i) => {
  if (c.still) inputs.push("-loop", "1", "-t", String(c.len + 0.5), "-i", c.file);
  else inputs.push("-ss", String(c.ss), "-i", c.file);
  const chain = [];
  if (c.scene && SPEED !== 1) chain.push(`setpts=PTS/${SPEED}`);
  // timestamps must start at 0 here: `t` drives the camera expressions and trim
  chain.push(`setpts=PTS-STARTPTS`, `scale=${W}:${H}:force_original_aspect_ratio=decrease`, `pad=${W}:${H}:(ow-iw)/2:(oh-ih)/2`, `setsar=1`, `fps=${FPS}`);
  if (c.scene && process.env.DEBUG_TS) chain.push(`drawtext=text='t=%{pts\\:flt}':fontsize=64:fontcolor=yellow:x=40:y=900:box=1:boxcolor=black`);
  if (c.scene) {
    // Camera: zoom factor z(t) from the recorder's keyframes (smoothstep between
    // them) times a slow Ken Burns drift; the crop keeps the keyframe's focus point
    // where it was on screen. scale/crop accept `t` with eval=frame.
    // zoompan keeps a fixed output size (dynamic scale+crop makes ffmpeg re-buffer on
    // every size change and content drifts from its timestamps). It has no `t`, only
    // the input frame index `in`; the stream is CFR here so t = in/FPS.
    const { z, px, py } = cameraExpr(c);
    const T = (e) => e.replace(/\bt\b/g, `(in/${FPS})`);
    if (process.env.DEBUG) console.log("cam", c.file, "\n  z =", z, "\n  px =", px, "py =", py);
    chain.push(`zoompan=z='${T(z)}':x='clip(${T(px)}*(1-1/zoom),0,iw-iw/zoom)':y='clip(${T(py)}*(1-1/zoom),0,ih-ih/zoom)':d=1:s=${W}x${H}:fps=${FPS}`);
  }
  // fps last: setpts/trim leave the frame rate "unknown", and xfade insists on CFR
  // tpad clones the last frame so a clip a few frames shorter than its beat-snapped
  // slot can't starve the xfade chain (which would freeze/black the rest of the reel)
  chain.push(`tpad=stop_mode=clone:stop_duration=3`, `trim=duration=${c.len.toFixed(3)}`, `setpts=PTS-STARTPTS`, `format=yuv420p`, `fps=${FPS}`);
  filters.push(`[${i}:v]${chain.join(",")}[v${i}]`);
});
// xfade chain: each transition eats XFADE seconds
let last = "v0", offset = 0;
for (let i = 1; i < clips.length; i++) {
  offset += clips[i - 1].len - XFADE;
  const outLabel = i === clips.length - 1 ? "vout" : `x${i}`;
  const kind = clips[i].card || clips[i - 1].card ? "fade" : "smoothleft";
  filters.push(`[${last}][v${i}]xfade=transition=${kind}:duration=${XFADE}:offset=${offset.toFixed(3)}[${outLabel}]`);
  last = outLabel;
}
if (clips.length === 1) filters.push(`[v0]copy[vout]`);
const total = clips.reduce((a, c) => a + c.len, 0) - XFADE * (clips.length - 1);

const args = [FFMPEG, "-v", "warning", "-y", ...inputs];
let maps = ["-map", "[vout]"];
if (existsSync(MUSIC)) {
  args.push("-i", MUSIC);
  filters.push(`[${clips.length}:a]atrim=0:${total.toFixed(3)},asetpts=PTS-STARTPTS,afade=t=in:st=0:d=1.2,afade=t=out:st=${(total - 2.5).toFixed(3)}:d=2.5,volume=0.85,aformat=sample_rates=48000:channel_layouts=stereo[aout]`);
  maps = ["-map", "[vout]", "-map", "[aout]", "-c:a", "aac", "-b:a", "192k"];
}
args.push("-filter_complex", filters.join(";"), ...maps, "-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p", "-r", String(FPS), "-movflags", "+faststart", "-t", total.toFixed(3), `out/${OUT}`);

console.log(`${clips.length} clips · beat ${beat.toFixed(3)}s (${(60 / beat).toFixed(1)} bpm) · half-bar ${HALF_BAR.toFixed(3)}s · ${total.toFixed(1)}s total`);
for (const c of clips) console.log(`  ${c.card ? "card " : "scene"} ${c.file.split("/").pop().padEnd(22)} ${c.len.toFixed(2)}s${c.raw ? ` (raw ${c.raw.toFixed(1)}s)` : ""}`);
const t0 = Date.now();
execFileSync(args[0], args.slice(1), { stdio: "inherit" });
console.log(`→ out/${OUT}  (${(Date.now() - t0) / 1000 | 0}s encode)`);
