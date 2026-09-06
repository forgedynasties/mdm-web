import React from "react";
import { Composition, staticFile } from "remotion";
import { Chapter, ChapterProps, FPS, chapterDuration } from "./Chapter";

// The chapter to render is chosen with --props='{"chapter":"ch01"}' (default ch01).
// Assets live in public/<chapter>/: take.webm, take.json, narration.json, audio/*.wav
export const Root: React.FC = () => (
  <Composition
    id="Chapter"
    component={Chapter}
    width={1920}
    height={1080}
    fps={FPS}
    durationInFrames={30 * FPS}
    defaultProps={{ chapter: "ch01" } as ChapterProps}
    calculateMetadata={async ({ props }) => {
      const take = await fetch(staticFile(`${props.chapter}/take.json`)).then((r) => r.json());
      const narration = await fetch(staticFile(`${props.chapter}/narration.json`)).then((r) => r.json());
      return { durationInFrames: chapterDuration(take, narration), props: { ...props, take, narration } };
    }}
  />
);
