# ChatGPT Voice over a phone call

Call your FreeCall number from any phone and have a spoken conversation with
ChatGPT using your **existing ChatGPT subscription** — no OpenAI API key. The
browser drives the consumer web UI (chatgpt.com voice mode), exactly like a
human tapping the voice button.

```
Phone ──FreeCall──▶ MikoPBX ──AudioSocket 8 kHz slin──▶ bridge (virtual mode)
                                                          │ pacat / parec
                                                          ▼
                                             PulseAudio (audio service)
                    audiosocket_mic sink ──monitor──▶ audiosocket_mic_source
                                                          ▲ default source
                                                          │
                       Camoufox (chatgpt.com, Voice Mode)
                                                          │ default sink
                                                          ▼
                    audiosocket_spk sink ◀───────── Firefox playback
```

- The **bridge** (Go, unchanged) owns the phone side: AudioSocket framing,
  pacing, jitter buffers, and 8 kHz ↔ 48 kHz resampling.
- The **audio** container runs PulseAudio with two null sinks and a remapped
  microphone source. It pins the defaults so any browser lands on the right
  devices without device pickers: default sink `audiosocket_spk`, default
  source `audiosocket_mic_source`.
- The **camoufox** container runs Camoufox (anti-detect Firefox) on a private
  Xvfb display with a persistent profile volume. Its controller polls the
  bridge's `/stats` endpoint: a call arrives → tap **Tap to talk**; the call
  ends → **End call** → reload to a fresh page, so every call is a new
  conversation.
- Nothing here uses the OpenAI API or any undocumented private endpoint. It is
  a browser, a web page, and the buttons on it.

The echo mode and the rest of the AudioSocket implementation are untouched;
set `BRIDGE_MODE=echo` and the stack behaves exactly as before.

## How the web page behaves (measured, Nov 2026)

The browser idles on `https://chatgpt.com/` and opens
`https://chatgpt.com/?mode=voice` only while a call is active, so no
session runs between calls. Verified against the live site:

- **No login required to test**: the anonymous page offers a single
  **Tap to talk** button. One tap starts a GPT-Live session; no tap is needed
  again until the session is ended.
- The live overlay is a `div[data-testid="avatar-overlay-voice-orb"]` orb
  with two text-labelled controls: **Mute** and **End call**. None of the
  voice controls carry ids or aria-labels; the controller finds them by role
  and text, which survived this DOM.
- After **End call** the orb element stays in the DOM wrapped in an
  `invisible` (visibility:hidden) container, so the controller checks
  visibility, not element count, when deciding whether a session is over.
- **End call** returns to the idle page. Reloading (or the next tap) starts a
  *fresh* conversation — this is what makes "new call = new chat" true.
- With a **logged-in profile** the same page runs under your plan's Live
  allowance (Plus: ~3 h per rolling 24 h) instead of the anonymous free tier,
  and transcripts land in your chat history.

## Requirements

- A ChatGPT plan with web Voice access if you log in (Free works
  anonymously; Plus gives a bigger allowance).
- The MikoPBX dialplan from [mikopbx.md](mikopbx.md) pointing at `bridge:9092`.
- Enough disk for the camoufox image (~2 GB; the pinned Camoufox browser build
  is downloaded at image build time).

## Optional: one-time login

Anonymous voice works out of the box — start there. To use your subscription
instead, log the browser profile in once through noVNC:

1. In `.env` set `CF_LOGIN_ONLY=true`, then `docker compose up -d --build`.
2. Publish port `6080` temporarily (Coolify: add a port mapping; plain
   Docker: see below) and browse to `http://<host>:6080/vnc.html`.
3. Log in to ChatGPT in the Firefox window: email, password, the
   Cloudflare/Turnstile check, any 2FA. Solve captchas like a human; that is
   why this step is interactive.
4. Watch for confirmation:

   ```sh
   docker compose logs -f camoufox
   # login state: in
   ```

5. Un-publish `6080`, set `CF_LOGIN_ONLY=false`, and
   `docker compose up -d camoufox`.

Session cookies live in the `camoufox-data` volume; they survive restarts
and upgrades. Treat that volume as a secret. If ChatGPT ever logs the session
out, repeat this procedure.

## Making a call

Dial the FreeCall number that reaches the `audiosocket-bridge` dialplan
extension. A few seconds after connect you hear ChatGPT's greeting. Speak
normally; this is the same GPT-Live conversation you get on the website,
including interruptions. Hanging up ends the voice session and the next call
starts a fresh conversation. The round trip adds roughly 80–250 ms over the
phone network (see the latency budget in [audio-pipeline.md](audio-pipeline.md)).

## Verification before the first call

Prove the audio path end to end **without** involving ChatGPT:

```sh
docker compose up -d --build
scripts/verify-audio.sh
```

The script checks, in order: devices and pinned defaults; the AudioSocket
loop with the built-in tester while a sine stands in for browser output
(the returned frames must carry the tone to the caller); the caller-audio
path into the browser's microphone source; and the controller's login and
readiness state. Every step prints PASS/FAIL with measured dBFS levels.

Then watch one real call happen:

```sh
docker compose exec bridge wget -qO- http://camoufox:8080/statusz
# {"ready": true, "logged_in": false, "call_active": false, ...}
```

Place the call: `call_active` flips true, `voice_active` follows a few
seconds later, both drop on hangup.

## Observability and recovery

| What | Where |
| --- | --- |
| Call counters, frame/byte stats, drops, levels | `bridge:8080/stats`, `/metrics` |
| Bridge readiness (endpoint healthy) | `bridge:8080/readyz` |
| Browser login/voice state, restarts, last error | `camoufox:8080/statusz` |
| Selector misses | screenshots + HTML in `camoufox-data` under `/data/debug` |

Recovery behavior:

- `pacat`/`parec` and the call loop are supervised by the bridge with
  exponential backoff (existing behavior).
- The caller hears baked spoken prompts: **connected** (chime + "Connected.
  You can speak now.") before the call bridges, **error** if the audio
  backend fails, and **busy** before the second caller is rejected. Set
  `BRIDGE_PROMPTS=false` to disable; regenerate the clips with
  `cmd/prompt-tts` (see the command's `-h`).
- The controller restarts the browser after repeated page failures
  (`CF_PAGE_FAILURES_BEFORE_RESTART`, default 5), closes stray voice
  overlays between calls so the allowance is never burned silently, and
  reloads to a fresh page after every call.
- The call can run until hangup (`BRIDGE_READ_TIMEOUT=0`,
  `BRIDGE_MAX_CALL_DURATION=0` by default); set either to bound it.
- All three containers have Docker healthchecks; Coolify restarts a service
  whose healthcheck fails.

## If OpenAI changes the page

The controller finds the entry button through layered selectors
(`data-testid`, `aria-label`, role+name, button text) and verified that the
text-only controls match by role. When a selector misses, the controller logs
`voice-button-missing` (or the end-control equivalent) and saves a screenshot
plus the page HTML to `/data/debug`. Copy a selector from that HTML into
`.env` without rebuilding:

```sh
CF_VOICE_SELECTOR='button:has-text("Talk")'
CF_END_SELECTOR='button:has-text("End call")'
docker compose up -d camoufox
```

## Limits and honest caveats

- **Voice usage is plan-limited** (anonymous: a small free allowance that
  today's testing can exhaust — ChatGPT then shows a
  `modal-voice-rate-limit` dialog, which the controller recognizes, closes
  and backs off from for 10 minutes; Plus: ~3 h per rolling 24 h under
  your login). Sessions end with the call and the page reloads, so idle
  time is not billed. When `/statusz` shows `"voice_limited": true`, either
  wait for the free-tier reset or complete the one-time login.
- **One voice conversation at a time** — matches the bridge's one-call slot;
  a second caller gets a busy signal from the existing implementation.
- **Firefox support**: OpenAI officially lists Chrome/Safari/Edge for web
  Voice. The anonymous voice page was verified working in Camoufox (a
  Firefox fork) in November 2026. Residual risk: OpenAI could break Firefox
  support. The simplest fallback then is the same controller on a patched
  Chromium against the identical PulseAudio devices; the bridge and audio
  stack would need no changes, only the ~150-line browser layer.
- **Detection**: Camoufox looks like a real browser and the profile holds a
  real session, but OpenAI may still flag automation someday. Nothing here
  bypasses a paywall; it uses entitlements you already have.
- **Audio leg proven locally up to the browser** (protocol loop with the
  built-in tester, voice session started and ended by the controller on the
  live site). The PulseAudio round trip needs the Linux deploy host; run
  `scripts/verify-audio.sh` there.
