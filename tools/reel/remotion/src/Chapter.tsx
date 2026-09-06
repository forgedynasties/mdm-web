import React from "react";
import { AbsoluteFill, Audio, OffthreadVideo, Sequence, interpolate, spring, staticFile, useCurrentFrame, useVideoConfig } from "remotion";

export const FPS = 30;
const INTRO_MIN = 3.2; // seconds, extended to the intro narration length

type Step = { id: string; text: string; in: number; out: number };
export type Take = { id: string; number: number; title: string; intro: string; steps: Step[] };
export type Narration = { intro: { file: string; dur: number }; steps: Record<string, { file: string; dur: number }> };
export type ChapterProps = { chapter: string; take?: Take; narration?: Narration };

const secToF = (s: number) => Math.round(s * FPS);
export const chapterDuration = (take: Take, narration: Narration) => {
  const intro = Math.max(INTRO_MIN, narration.intro.dur + 0.8);
  const steps = take.steps.reduce((a, s) => a + (s.out - s.in) / 1000, 0);
  return secToF(intro + steps + 1.5);
};

const ACCENT = "#e74c3c";
const font = "Inter, system-ui, sans-serif";
const display = "Poppins, Inter, system-ui, sans-serif";

// ---------------------------------------------------------------- chapter title
const TitleCard: React.FC<{ take: Take }> = ({ take }) => {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const up = (delay: number) => {
    const s = spring({ frame: frame - delay, fps, config: { damping: 18, stiffness: 90 } });
    return { opacity: s, transform: `translateY(${(1 - s) * 40}px)` };
  };
  return (
    <AbsoluteFill style={{ background: "linear-gradient(135deg,#0f1115 0%,#1a1d26 100%)", color: "#fff", fontFamily: font }}>
      <div style={{ position: "absolute", left: 160, top: 0, bottom: 0, width: 6, background: ACCENT, transform: `scaleY(${spring({ frame, fps, config: { damping: 20 } })})`, transformOrigin: "top" }} />
      <div style={{ position: "absolute", left: 210, top: 330 }}>
        <div style={{ ...up(4), fontSize: 26, letterSpacing: "0.32em", textTransform: "uppercase", color: ACCENT, marginBottom: 26 }}>AIO MDM · Training</div>
        <div style={{ ...up(10), fontFamily: display, fontWeight: 700, fontSize: 112, lineHeight: 1.05, letterSpacing: "-0.02em" }}>Chapter {take.number}</div>
        <div style={{ ...up(16), fontFamily: display, fontWeight: 700, fontSize: 72, lineHeight: 1.1, color: "#c9cdd6", marginTop: 8 }}>{take.title}</div>
        <div style={{ ...up(24), fontSize: 34, color: "#8f95a3", marginTop: 34 }}>{take.steps.length} steps · {Math.round(take.steps.reduce((a, s) => a + (s.out - s.in), 0) / 60000)} min</div>
      </div>
      <div style={{ position: "absolute", right: 120, bottom: 80, fontFamily: display, fontWeight: 700, fontSize: 34 }}><span style={{ color: ACCENT }}>AIO</span> MDM</div>
    </AbsoluteFill>
  );
};

// ---------------------------------------------------------------- one step
const StepView: React.FC<{ take: Take; step: Step; index: number; video: string }> = ({ take, step, index, video }) => {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const durF = secToF((step.out - step.in) / 1000);
  const fadeIn = interpolate(frame, [0, 8], [0, 1], { extrapolateRight: "clamp" });
  const capIn = spring({ frame, fps, config: { damping: 16, stiffness: 120 } });
  return (
    <AbsoluteFill style={{ background: "#0f1115", fontFamily: font }}>
      {/* the take, framed */}
      <div style={{ position: "absolute", left: 64, top: 84, width: 1792, height: 800, borderRadius: 18, overflow: "hidden", boxShadow: "0 30px 80px rgba(0,0,0,.55)", opacity: fadeIn }}>
        <OffthreadVideo src={video} startFrom={secToF(step.in / 1000)} endAt={secToF(step.out / 1000) + 2} muted style={{ width: 1792, height: 800 }} />
      </div>
      {/* chapter chip + progress */}
      <div style={{ position: "absolute", left: 64, top: 26, display: "flex", alignItems: "center", gap: 18, color: "#c9cdd6", fontSize: 24 }}>
        <span style={{ fontFamily: display, fontWeight: 700, color: "#fff" }}>Chapter {take.number} · {take.title}</span>
        <span style={{ color: "#6b7280" }}>step {index + 1} of {take.steps.length}</span>
        <div style={{ display: "flex", gap: 6, marginLeft: 8 }}>
          {take.steps.map((s, i) => <div key={s.id} style={{ width: i === index ? 34 : 12, height: 8, borderRadius: 4, background: i < index ? "#6b7280" : i === index ? ACCENT : "#2a2f3a" }} />)}
        </div>
      </div>
      <div style={{ position: "absolute", right: 64, top: 26, fontFamily: display, fontWeight: 700, fontSize: 24, color: "#fff" }}><span style={{ color: ACCENT }}>AIO</span> MDM</div>
      {/* caption bar */}
      <div style={{ position: "absolute", left: 64, right: 64, bottom: 44, opacity: capIn, transform: `translateY(${(1 - capIn) * 24}px)` }}>
        <div style={{ display: "flex", alignItems: "flex-start", gap: 22, background: "rgba(255,255,255,.06)", border: "1px solid rgba(255,255,255,.1)", borderRadius: 16, padding: "22px 30px" }}>
          <div style={{ flex: "none", width: 44, height: 44, borderRadius: 22, background: ACCENT, color: "#fff", fontFamily: display, fontWeight: 700, fontSize: 22, display: "flex", alignItems: "center", justifyContent: "center" }}>{index + 1}</div>
          <div style={{ fontSize: 34, lineHeight: 1.3, color: "#f3f4f6" }}>{step.text}</div>
        </div>
      </div>
      {/* step progress line along the bottom of the video */}
      <div style={{ position: "absolute", left: 64, width: 1792, top: 884, height: 4, background: "#1f2430" }}>
        <div style={{ width: `${(frame / durF) * 100}%`, height: 4, background: ACCENT }} />
      </div>
    </AbsoluteFill>
  );
};

// ---------------------------------------------------------------- chapter
export const Chapter: React.FC<ChapterProps> = ({ chapter, take, narration }) => {
  if (!take || !narration) return null;
  const video = staticFile(`${chapter}/take.webm`);
  const introF = secToF(Math.max(INTRO_MIN, narration.intro.dur + 0.8));
  let cursor = introF;
  return (
    <AbsoluteFill style={{ background: "#0f1115" }}>
      <Sequence from={0} durationInFrames={introF}>
        <TitleCard take={take} />
        <Audio src={staticFile(`${chapter}/audio/${narration.intro.file}`)} />
      </Sequence>
      {take.steps.map((step, i) => {
        const from = cursor;
        const durF = secToF((step.out - step.in) / 1000);
        cursor += durF;
        return (
          <Sequence key={step.id} from={from} durationInFrames={durF}>
            <StepView take={take} step={step} index={i} video={video} />
            <Audio src={staticFile(`${chapter}/audio/${narration.steps[step.id].file}`)} />
          </Sequence>
        );
      })}
    </AbsoluteFill>
  );
};
