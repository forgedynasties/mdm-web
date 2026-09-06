// Chapter 8 — Remote support: take over a screen.
export default {
  id: "ch08",
  number: 8,
  title: "Remote support",
  intro: "Chapter eight. Remote support. Taking over a device screen from the browser.",
  theme: "light",
  login: true,
  steps: [
    { id: "button", text: "Remote Control is on the device page. You need the operator role and the device must be online.",
      run: async (s) => { await s.page.goto(s.BASE + "/devices/" + s.HERO, { waitUntil: "load" }); await s.zoom(); await s.hold(800); await s.glide("a:has-text('Remote Control')", { dur: 1000 }); } },
    { id: "session", text: "The session starts in a few seconds. What you see is the live screen; the frame rate adapts to the venue's Wi-Fi.",
      run: async (s) => { await s.click("a:has-text('Remote Control')"); await s.page.waitForURL(/\/remote/, { timeout: 15000 }).catch(() => {}); await s.reloaded(); await s.page.locator("#rc-canvas").waitFor({ state: "visible", timeout: 20000 }).catch(() => {}); await s.hold(1500); } },
    { id: "tap", text: "Click on the screen to tap. Drag to swipe. Everything goes straight to the device, so be as careful as you would be standing in front of it.",
      run: async (s) => { await s.click("#rc-canvas", { offset: { x: -160, y: -40 } }); await s.hold(1500); await s.click("#rc-canvas", { offset: { x: 120, y: 60 } }); await s.hold(1500); } },
    { id: "keys", text: "The hardware keys panel sends Home, Back and volume. Turn on keyboard capture to type into a field on the device.",
      run: async (s) => { await s.glide("text=Hardware keys >> nth=0", { dur: 900 }).catch(() => {}); await s.hold(700); await s.glide("text=Keyboard >> nth=0", { dur: 800 }).catch(() => {}); await s.hold(600); } },
    { id: "one", text: "One session per device. If a colleague opens the same device, they take over and you are disconnected; that is by design.",
      run: async (s) => { await s.glide("#rc-canvas", { dur: 800 }).catch(() => {}); await s.hold(1200); } },
    { id: "end", text: "End session when you are done. The device goes back to kiosk mode on its own; nothing is left running.",
      run: async (s) => { await s.click("#rc-disconnect").catch(() => {}); await s.hold(1800); } },
  ],
};
