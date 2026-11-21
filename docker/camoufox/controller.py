#!/usr/bin/env python3
"""Camoufox controller for the ChatGPT Voice bridge.

Keeps a persistent, authenticated Camoufox profile pointed at chatgpt.com,
watches the audiosocket-bridge health endpoint and starts/stops a ChatGPT
Voice session so it lines up with the phone call:

    FreeCall -> MikoPBX -> AudioSocket -> bridge (virtual mode) -> PulseAudio
                                                                   ^
                                    Camoufox mic/speaker <- default source/sink

The bridge owns all audio routing and resampling; this controller only
translates "call active" into "voice session active".

No OpenAI API is used. Only the consumer web UI, driven like a human would
drive it.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import signal
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

from camoufox.async_api import AsyncCamoufox
from playwright.async_api import TimeoutError as PlaywrightTimeoutError

# --------------------------------------------------------------------------
# Configuration
# --------------------------------------------------------------------------


def _env(name: str, default: str) -> str:
    value = os.environ.get(name, "").strip()
    return value if value else default


def _env_bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name, "").strip().lower()
    if not raw:
        return default
    return raw in ("1", "true", "yes", "on")


class Config:
    def __init__(self) -> None:
        self.profile_dir = _env("CF_PROFILE_DIR", "/data/profile")
        # The voice page, opened only while a call is active: logged in,
        # ?mode=voice auto-starts the voice session; anonymous shows a
        # "Tap to talk" button. Idling there would keep a session open
        # with nobody on the line, so the idle page is the same URL
        # without the mode parameter.
        self.voice_url = _env("CF_URL", "https://chatgpt.com/?mode=voice")
        parts = urlsplit(self.voice_url)
        idle_query = "&".join(
            part
            for part in parts.query.split("&")
            if part and not part.startswith("mode=")
        )
        self.idle_url = urlunsplit(parts._replace(query=idle_query))
        self.bridge_stats_url = _env("CF_BRIDGE_STATS_URL", "http://bridge:8080/stats")
        self.login_only = _env_bool("CF_LOGIN_ONLY", False)
        # Voice can run anonymously; set true to require a logged-in profile.
        self.require_login = _env_bool("CF_REQUIRE_LOGIN", False)
        self.poll_interval = float(_env("CF_POLL_MS", "500")) / 1000.0
        # How long to wait for the voice overlay to come up after clicking.
        self.voice_start_timeout = float(_env("CF_VOICE_START_TIMEOUT_S", "15"))
        # How long to wait for the voice overlay to go away after ending.
        self.voice_end_timeout = float(_env("CF_VOICE_END_TIMEOUT_S", "10"))
        self.page_failures_before_restart = int(_env("CF_PAGE_FAILURES_BEFORE_RESTART", "5"))
        self.restart_backoff_initial = float(_env("CF_RESTART_INITIAL_S", "5"))
        self.restart_backoff_max = float(_env("CF_RESTART_MAX_S", "60"))
        self.debug_dir = _env("CF_DEBUG_DIR", "/data/debug")
        self.display = _env("CF_DISPLAY", ":99")
        # Set CF_HEADLESS=true to run plain headless Firefox (no X display).
        self.headless = _env_bool("CF_HEADLESS", False)
        # Fingerprint OS. "linux" fits the container; override for dev hosts.
        self.os = _env("CF_OS", "linux")
        # Optional selector overrides for when OpenAI ships a new DOM.
        self.voice_selector = os.environ.get("CF_VOICE_SELECTOR", "").strip()
        self.end_selector = os.environ.get("CF_END_SELECTOR", "").strip()
        self.health_addr = ("", int(_env("CF_HEALTH_PORT", "8080")))


CFG = Config()

# Auto-grant getUserMedia (Playwright cannot grant microphone on Firefox),
# allow autoplay without a user gesture, keep WebRTC enabled (the voice
# session needs it).
FIREFOX_PREFS = {
    "media.navigator.permission.disabled": True,
    "media.autoplay.default": 0,
    "media.peerconnection.enabled": True,
}

log = logging.getLogger("controller")


def setup_logging() -> None:
    level = getattr(logging, _env("CF_LOG_LEVEL", "INFO").upper(), logging.INFO)
    logging.basicConfig(
        level=level,
        format="%(asctime)s %(levelname)s %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S%z",
    )


# --------------------------------------------------------------------------
# Status reporting (health server)
# --------------------------------------------------------------------------


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


class Status:
    """Thread-safe controller status, served as JSON on :8080."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._data = {
            "running": True,
            "ready": False,  # browser up, logged in, able to take a call
            "logged_in": False,
            "login_state": "unknown",
            "voice_active": False,
            "voice_limited": False,
            "call_active": False,
            "bridge_up": False,
            "page_url": "",
            "last_error": "",
            "consecutive_failures": 0,
            "voice_sessions_started": 0,
            "voice_sessions_ended": 0,
            "browser_restarts": 0,
            "login_only": CFG.login_only,
            "updated": _now_iso(),
        }

    def update(self, **fields) -> None:
        with self._lock:
            self._data.update(fields)
            self._data["updated"] = _now_iso()

    def snapshot(self) -> dict:
        with self._lock:
            return dict(self._data)


STATUS = Status()


class HealthHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802 - http.server API
        path = self.path.split("?", 1)[0]
        if path == "/healthz":
            body = b"ok\n"
            self._respond(200, "text/plain", body)
        elif path == "/statusz":
            body = json.dumps(STATUS.snapshot(), indent=2).encode() + b"\n"
            self._respond(200, "application/json", body)
        else:
            self._respond(404, "text/plain", b"not found\n")

    def _respond(self, code: int, content_type: str, body: bytes) -> None:
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args) -> None:  # keep the log clean
        pass


def start_health_server() -> ThreadingHTTPServer:
    server = ThreadingHTTPServer(CFG.health_addr, HealthHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True, name="health")
    thread.start()
    return server


# --------------------------------------------------------------------------
# Bridge polling
# --------------------------------------------------------------------------


def _fetch_bridge_stats() -> dict | None:
    """Fetch the bridge /stats JSON. Returns None when unreachable."""
    try:
        with urllib.request.urlopen(CFG.bridge_stats_url, timeout=3) as response:
            return json.loads(response.read().decode())
    except (urllib.error.URLError, OSError, ValueError):
        return None


async def call_active() -> bool | None:
    """True/False when the bridge answers, None when it does not."""
    stats = await asyncio.to_thread(_fetch_bridge_stats)
    if stats is None:
        return None
    return stats.get("active_call") is not None


# --------------------------------------------------------------------------
# Page interaction
# --------------------------------------------------------------------------

VOICE_BUTTON_SELECTORS = [
    # Composer entry, measured on the live site (anonymous and logged-in).
    'button[aria-label="Start Voice"]',
]
# The anonymous landing page exposes "Tap to talk" as the accessible name.

VOICE_NAME_RE = re.compile(r"^(?:tap to talk|(?:start|use|enter)(?: new)? voice(?: mode| chat)?|voice mode)$", re.I)

# The session controls are text-labelled ("End call") with no stable
# testids. Loose attribute matching is unsafe here: "close" also matches
# the sidebar's close-sidebar-button and "end" the composer's Send, so
# the end control is located by accessible name only.
END_NAME_RE = re.compile(r"^(?:end|close) (?:call|chat|voice(?: chat| mode)?)$", re.I)

# Login detection. Every ChatGPT document embeds the bootstrap session
# payload in a <script> tag. Its federatedLogoutUrl is null for anonymous
# visitors and a real auth.openai.com URL once a session exists -- verified
# in both states, including mid-voice-session, on the live site (saved DOMs:
# loggedin.html, loggedout2.html, vchat.html). Plain text matching cannot
# replace it: the embedded JSON contains "login" strings either way
# (loginWebUrl, login page variants), so the old text-based check
# misclassified logged-in pages.
FEDERATED_LOGOUT_RE = re.compile(r'"federatedLogoutUrl"\s*:\s*(null|"[^"]*")')
CHALLENGE_RE = re.compile(r"verify you are human|just a moment|cf-challenge", re.I)
LOGIN_CONTROL_RE = re.compile(r"^(?:log ?in|sign ?up)$", re.I)

VOICE_RATE_LIMIT_SELECTORS = [
    '[data-testid="modal-voice-rate-limit"]',
]

# Quiet period after the voice rate-limit modal appears (anonymous free
# tier exhausted); retrying sooner just reopens the modal.
RATE_LIMIT_BACKOFF_S = 600.0


async def _first_visible(page, selectors: list[str], role: tuple[str, re.Pattern] | None = None):
    """Return the first visible locator among the candidates, else None."""
    for sel in selectors:
        try:
            locator = page.locator(sel).filter(visible=True).first
            if await locator.is_visible():
                return locator
        except Exception:
            continue
    if role is not None:
        try:
            locator = page.get_by_role(role[0], name=role[1]).filter(visible=True).first
            if await locator.is_visible():
                return locator
        except Exception:
            pass
    return None


async def find_voice_button(page):
    if CFG.voice_selector:
        locator = await _first_visible(page, [CFG.voice_selector])
        if locator is not None:
            return locator
    button = await _first_visible(page, VOICE_BUTTON_SELECTORS)
    if button is not None:
        return button
    return await _first_visible(
        page, [], role=("button", VOICE_NAME_RE)
    )


async def find_end_control(page):
    if CFG.end_selector:
        locator = await _first_visible(page, [CFG.end_selector])
        if locator is not None:
            return locator
    return await _first_visible(page, [], role=("button", END_NAME_RE))


async def detect_login(page) -> str:
    """'in', 'out' or 'unknown'."""
    try:
        if urlsplit(page.url).path.rstrip("/") == "/auth/login":
            return "out"
        try:
            html = await page.content()
        except Exception:
            html = ""
        if CHALLENGE_RE.search(html):
            return "unknown"
        match = FEDERATED_LOGOUT_RE.search(html)
        if match:
            return "out" if match.group(1) == "null" else "in"
        # No payload yet (page still loading or a stripped view): fall
        # back to visible login controls. An unresolved page stays unknown.
        for role in ("link", "button"):
            if await _first_visible(page, [], role=(role, LOGIN_CONTROL_RE)):
                return "out"
    except Exception:
        return "unknown"
    return "unknown"


async def voice_rate_limited(page) -> bool:
    """True when the voice rate-limit modal is on the page."""
    try:
        for selector in VOICE_RATE_LIMIT_SELECTORS:
            if await page.locator(selector).filter(visible=True).first.is_visible():
                return True
        return await page.get_by_text(re.compile(r"^Voice limit reached\b", re.I)).filter(visible=True).first.is_visible()
    except Exception:
        return False


async def voice_overlay_present(page) -> bool:
    """Best-effort check that a voice session overlay is on the page.

    Only two signals count, both verified against the live site:
      - an audio element driven by a MediaStream and playing,
      - the live orb container (exact testid), visible. The orb element
        stays in the DOM after the session ends -- measured closed
        state: the wrapper gains the `invisible` class
        (visibility:hidden) -- so count() alone would never report the
        session as ended.
    """
    try:
        playing = await page.evaluate(
            """() => {
                for (const a of document.querySelectorAll('audio')) {
                    if (a.srcObject && !a.paused) return true;
                }
                return false;
            }"""
        )
        if playing:
            return True
        orb = page.get_by_test_id("avatar-overlay-voice-orb").filter(visible=True).first
        return await orb.is_visible()
    except Exception:
        return False


async def click(locator):
    """Click with a Playwright click, then a synthetic DOM click as fallback."""
    try:
        await locator.click(timeout=5000)
        return True
    except Exception:
        try:
            await locator.evaluate("el => el.click()")
            return True
        except Exception:
            return False


async def save_debug(page, tag: str) -> None:
    """Screenshot + HTML snapshot so a selector miss can be diagnosed."""
    try:
        os.makedirs(CFG.debug_dir, exist_ok=True)
        stamp = time.strftime("%Y%m%dT%H%M%S")
        base = os.path.join(CFG.debug_dir, f"{tag}-{stamp}")
        await page.screenshot(path=f"{base}.png")
        html = await page.content()
        with open(f"{base}.html", "w", encoding="utf-8") as handle:
            handle.write(html)
        log.info("debug snapshot saved: %s", base)
    except Exception as exc:  # pragma: no cover - best effort only
        log.warning("debug snapshot failed: %s", exc)


# --------------------------------------------------------------------------
# Voice session control
# --------------------------------------------------------------------------


class VoiceController:
    """Starts and ends ChatGPT Voice sessions on one open page."""

    def __init__(self, page) -> None:
        self.page = page
        self.active = False
        self.failures = 0
        self.limit_hit = False
        self.limit_until = 0.0
        self.blocked = False

    async def start(self) -> bool:
        page = self.page
        if time.monotonic() < self.limit_until:
            await self.block("limit", "ChatGPT Voice usage limit reached")
            return False
        if self.blocked:
            return False
        deadline = time.monotonic() + CFG.voice_start_timeout
        clicked = False
        setup_steps = set()
        while time.monotonic() < deadline:
            if await voice_rate_limited(page):
                await self._dismiss_rate_limit(page)
                self.limit_hit = True
                self.limit_until = time.monotonic() + RATE_LIMIT_BACKOFF_S
                STATUS.update(voice_limited=True)
                await self.block("limit", "ChatGPT Voice usage limit reached")
                return False
            for clip, message in (
                ("microphone", r"^Microphone permission denied$"),
                ("unavailable", r"^(?:Voice chat couldn['’]t start|Unable to start voice chat|Voice unavailable(?: [—–-] reload to try again)?)$"),
            ):
                error = page.get_by_text(re.compile(message, re.I)).filter(visible=True).first
                if await error.is_visible():
                    await self.block(clip, await error.inner_text())
                    return False
            if await voice_overlay_present(page):
                connecting = page.get_by_text(re.compile(r"^Connecting(?:…|\.\.\.)$", re.I))
                if await connecting.filter(visible=True).first.is_visible():
                    await asyncio.sleep(0.25)
                    continue
                self.active = True
                self.failures = 0
                self.limit_hit = False
                STATUS.update(voice_limited=False, last_error="")
                log.info("voice session started")
                return True

            setup = page.get_by_role("dialog", name="Set up voice chat", exact=True).first
            if await setup.is_visible():
                for label in ("Start voice chat", "Next"):
                    control = setup.get_by_role("button", name=label, exact=True).first
                    if label not in setup_steps and await control.is_visible() and await control.is_enabled():
                        # Use the selected voice and leave optional sharing settings alone.
                        try:
                            await control.click(timeout=1500)
                        except PlaywrightTimeoutError:
                            break
                        setup_steps.add(label)
                        clicked = True
                        break
            elif not clicked:
                button = await find_voice_button(page)
                if button is not None and await button.is_enabled():
                    clicked = await click(button)
                    if not clicked:
                        break
            await asyncio.sleep(0.25)

        setup = page.get_by_role("dialog", name="Set up voice chat", exact=True).first
        if await setup.is_visible():
            await self.block("setup", "ChatGPT Voice setup could not be completed")
        elif await page.get_by_role("heading", name="Log in or sign up").first.is_visible():
            await self.block("login", "ChatGPT Voice requires login")
        else:
            await self.block("unavailable", "ChatGPT Voice startup did not complete")
        await save_debug(page, "voice-start-blocked")
        return False

    async def block(self, clip: str, reason: str) -> None:
        if self.blocked:
            return
        self.blocked = True
        self.active = False
        STATUS.update(last_error=reason, voice_active=False)
        log.warning("%s", reason)
        if await call_active() is not True:
            return
        path = Path(__file__).with_name("announcements") / f"{clip}.wav"
        try:
            player = await asyncio.create_subprocess_exec(
                "paplay", "--device=audiosocket_spk", str(path)
            )
        except OSError:
            log.exception("could not start caller announcement")
            return
        try:
            if await asyncio.wait_for(player.wait(), timeout=15):
                log.warning("caller announcement failed: %s", clip)
        except asyncio.TimeoutError:
            log.warning("caller announcement timed out: %s", clip)
        finally:
            if player.returncode is None:
                player.kill()
                await player.wait()

    async def _dismiss_rate_limit(self, page) -> None:
        try:
            close = page.locator(
                '[data-testid="modal-voice-rate-limit"] [data-testid="close-button"]'
            )
            if await close.first.is_visible():
                await click(close.first)
        except Exception:
            pass

    async def end(self) -> bool:
        """End the voice session by leaving the voice page.

        Navigation tears down the media connection on both the anonymous
        and the logged-in session UIs. Measured the hard way: the "End
        Voice" control has no stable testid, and loose "close"/"end"
        attribute matches hit the sidebar and the composer instead.
        """
        page = self.page
        try:
            await page.goto(CFG.idle_url, wait_until="domcontentloaded", timeout=60_000)
        except Exception as exc:
            log.warning("could not navigate away from the voice session: %s", exc)
            return False

        deadline = time.monotonic() + CFG.voice_end_timeout
        while time.monotonic() < deadline:
            if not await voice_overlay_present(page):
                self.active = False
                STATUS.update(voice_active=False)
                log.info("voice session ended")
                return True
            await asyncio.sleep(0.5)

        await save_debug(page, "voice-end-timeout")
        log.warning("voice session did not end within %ss", CFG.voice_end_timeout)
        return False

    async def recover(self) -> None:
        """End a stale voice session (call already gone)."""
        if await voice_overlay_present(self.page):
            log.info("ending stale voice session")
            await self.end()


# --------------------------------------------------------------------------
# Main loop
# --------------------------------------------------------------------------


async def session_loop(context, stop: asyncio.Event) -> None:
    """Drive one browser context until it breaks or stop is requested."""
    page = context.pages[0] if context.pages else await context.new_page()
    voice = VoiceController(page)
    failures = 0
    last_login_state = None
    # True while the page sits on the voice URL. Navigated to only when
    # a call is active; every other goto returns to the idle page.
    on_voice_page = False

    await page.goto(CFG.idle_url, wait_until="domcontentloaded", timeout=60_000)
    log.info("opened %s (idle)", CFG.idle_url)

    while not stop.is_set():
        try:
            login_state = await detect_login(page)
            if login_state != last_login_state:
                log.info("login state: %s", login_state)
                last_login_state = login_state
            STATUS.update(
                logged_in=login_state == "in",
                login_state=login_state,
                page_url=page.url,
                consecutive_failures=failures,
            )

            if CFG.login_only:
                STATUS.update(ready=login_state == "in")
                await asyncio.sleep(CFG.poll_interval)
                failures = 0
                continue

            active = await call_active()
            STATUS.update(
                call_active=bool(active),
                bridge_up=active is not None,
            )

            if active is None:
                # Bridge unreachable: make sure we are not burning the
                # ChatGPT voice session while nobody is on the line.
                # voice.recover() ends the session by leaving the voice
                # page; a failed navigation is retried next poll.
                if voice.active or await voice_overlay_present(page):
                    await voice.recover()
                    on_voice_page = False
                await asyncio.sleep(CFG.poll_interval)
                continue

            if not active and not voice.active and await voice_overlay_present(page):
                # A leaked overlay: leaving the page tears it down and
                # stops the allowance burn; retried next poll if the
                # navigation fails.
                log.info("closing stray voice overlay between calls")
                await voice.recover()
                on_voice_page = False

            if not active:
                voice.blocked = False
                if on_voice_page and not voice.active:
                    on_voice_page = not await voice.end()

            if active and voice.blocked:
                if await voice_overlay_present(page):
                    on_voice_page = not await voice.end()
            elif active and not voice.active:
                if CFG.require_login and login_state != "in":
                    await voice.block("login", "ChatGPT Voice requires login")
                else:
                    if not on_voice_page:
                        # One navigation per call. While a call is active
                        # the page sits on the voice URL: logged in,
                        # ?mode=voice auto-starts the session; anonymous
                        # shows the "Tap to talk" button. Navigating
                        # again each poll would interrupt hydration, so
                        # retry voice.start() on the same page instead.
                        try:
                            await page.goto(
                                CFG.voice_url, wait_until="domcontentloaded", timeout=60_000
                            )
                            on_voice_page = True
                            log.info("opened %s for the call", CFG.voice_url)
                        except Exception as exc:
                            failures += 1
                            log.warning("could not open the voice page: %s", exc)
                    if on_voice_page and await voice.start():
                        sessions = STATUS.snapshot()["voice_sessions_started"] + 1
                        STATUS.update(
                            voice_active=True,
                            ready=True,
                            voice_sessions_started=sessions,
                        )
                    elif on_voice_page and not voice.blocked and not voice.limit_hit:
                        failures += 1

            elif not active and voice.active:
                # voice.end() leaves the voice page itself: navigation
                # tears the session down (no end control to find), and
                # the next call opens a fresh conversation.
                ended = await voice.end()
                if ended:
                    sessions = STATUS.snapshot()["voice_sessions_ended"] + 1
                    STATUS.update(voice_sessions_ended=sessions)
                    on_voice_page = False
                    log.info("returned to the idle page after the call")
                else:
                    failures += 1
                    # The page state is unknown after a failed goto;
                    # the next call navigates to the voice page anyway.
                    on_voice_page = False

            elif active and voice.active:
                # Watch for the overlay disappearing mid-call.
                if not await voice_overlay_present(page):
                    log.info("voice overlay vanished mid-call; restarting voice")
                    voice.active = False
                    if await voice.start():
                        STATUS.update(voice_active=True)
                failures = 0

            STATUS.update(
                ready=login_state == "in" and (voice.active or not active),
                consecutive_failures=failures,
            )

            if failures >= CFG.page_failures_before_restart:
                log.warning("%d consecutive failures; restarting browser", failures)
                raise RuntimeError("too many consecutive page failures")

            await asyncio.sleep(CFG.poll_interval)

        except RuntimeError:
            raise
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            failures += 1
            STATUS.update(
                last_error=str(exc),
                consecutive_failures=failures,
            )
            log.warning("page error (%d/%d): %s", failures,
                        CFG.page_failures_before_restart, exc)
            if failures >= CFG.page_failures_before_restart:
                raise
            await asyncio.sleep(CFG.poll_interval)


async def run(stop: asyncio.Event) -> None:
    backoff = CFG.restart_backoff_initial
    while not stop.is_set():
        try:
            launch = {
                "persistent_context": True,
                "user_data_dir": CFG.profile_dir,
                # Headed Firefox on our own Xvfb display (Linux container).
                # CF_HEADLESS=true gives plain headless Firefox instead, for
                # workstations without X.
                "headless": CFG.headless,
                "os": CFG.os,
                "block_webrtc": False,  # the voice session needs WebRTC
                "disable_coop": True,   # lets Turnstile's iframe be clicked
                # Real PulseAudio devices, not camoufox's generated fake
                # list. The voice session picks its microphone from
                # enumerateDevices(); with the fake list the outbound track
                # attaches to a spoofed device and sends zero frames
                # (measured: about:webrtc outbound-rtp 0.00 packetsSent/s
                # while plain getUserMedia captured the caller fine).
                "config": {"mediaDevices:enabled": False},
                "firefox_user_prefs": FIREFOX_PREFS,
            }
            if not CFG.headless:
                # The same display is served over VNC for the one-time login.
                launch["virtual_display"] = CFG.display
            async with AsyncCamoufox(**launch) as context:
                backoff = CFG.restart_backoff_initial
                await session_loop(context, stop)
                # Clean exit (stop requested): end any voice session.
                try:
                    page = context.pages[0] if context.pages else None
                    if page is not None:
                        voice = VoiceController(page)
                        if voice.active or await voice_overlay_present(page):
                            await voice.end()
                except Exception:
                    pass
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            if stop.is_set():
                break
            STATUS.update(
                last_error=str(exc),
                browser_restarts=STATUS.snapshot()["browser_restarts"] + 1,
            )
            log.error("browser session died: %s; restarting in %ss", exc, backoff)
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, CFG.restart_backoff_max)


async def main() -> None:
    setup_logging()
    server = start_health_server()
    log.info(
        "controller starting: voice_url=%s idle_url=%s bridge=%s "
        "login_only=%s profile=%s",
        CFG.voice_url,
        CFG.idle_url,
        CFG.bridge_stats_url,
        CFG.login_only,
        CFG.profile_dir,
    )

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stop.set)
        except (NotImplementedError, AttributeError):  # e.g. non-Linux dev host
            signal.signal(sig, lambda *_: stop.set())

    try:
        await run(stop)
    finally:
        STATUS.update(running=False, ready=False)
        log.info("controller stopped")
        server.shutdown()


if __name__ == "__main__":
    asyncio.run(main())
