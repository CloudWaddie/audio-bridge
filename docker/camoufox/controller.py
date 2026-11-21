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

from camoufox.async_api import AsyncCamoufox

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
        # ?mode=voice lands directly on the voice-capable page; it works
        # both logged in and anonymously (Free-tier Live access).
        self.url = _env("CF_URL", "https://chatgpt.com/?mode=voice")
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
    'button[data-testid*="voice" i]',
    'button[aria-label*="voice" i]',
    # Anonymous ?mode=voice landing page: the entry control has no id, no
    # testid and no aria-label; its stable surface is the button text.
    'button:has-text("Tap to talk")',
]

VOICE_NAME_RE = re.compile(r"voice|tap to talk", re.I)

VOICE_END_SELECTORS = [
    'button[data-testid*="close" i]',
    'button[aria-label*="end" i]',
    'button[aria-label*="close" i]',
]

END_NAME_RE = re.compile(r"end.*(chat|call|voice)|close.*(chat|call|voice)", re.I)

# Anonymous sessions also show the composer, so its presence alone proves
# nothing. These markers only count as logged in when the LOGOUT_MARKERS are
# absent.
COMPOSER_MARKERS = [
    "#prompt-textarea",
    'div[id="composer-background"]',
]

LOGOUT_MARKERS = [
    'button[data-testid="login-button"]',
    'button[data-testid="signup-button"]',
    'a[data-testid="login-link"]',
    'button[data-testid*="login" i]',
]

VOICE_RATE_LIMIT_SELECTORS = [
    '[data-testid="modal-voice-rate-limit"]',
    '[data-testid="blocking-initial-modals-done"] ~ [data-testid="close-button"]',
]

# Quiet period after the voice rate-limit modal appears (anonymous free
# tier exhausted); retrying sooner just reopens the modal.
RATE_LIMIT_BACKOFF_S = 600.0


async def _first_visible(page, selectors: list[str], role: tuple[str, re.Pattern] | None = None):
    """Return the first visible locator among the candidates, else None."""
    for sel in selectors:
        try:
            locator = page.locator(sel)
            if await locator.count() == 0:
                continue
            if await locator.first.is_visible():
                return locator.first
        except Exception:
            continue
    if role is not None:
        try:
            locator = page.get_by_role(role[0], name=role[1])
            if await locator.count() > 0 and await locator.first.is_visible():
                return locator.first
        except Exception:
            pass
    return None


async def find_voice_button(page):
    if CFG.voice_selector:
        locator = page.locator(CFG.voice_selector)
        if await locator.count() > 0:
            return locator.first
    button = await _first_visible(page, VOICE_BUTTON_SELECTORS)
    if button is not None:
        return button
    return await _first_visible(
        page, [], role=("button", VOICE_NAME_RE)
    )


async def find_end_control(page):
    if CFG.end_selector:
        locator = page.locator(CFG.end_selector)
        if await locator.count() > 0:
            return locator.first
    control = await _first_visible(page, VOICE_END_SELECTORS)
    if control is not None:
        return control
    return await _first_visible(page, [], role=("button", END_NAME_RE))


async def detect_login(page) -> str:
    """'in', 'out' or 'unknown'."""
    try:
        if "/auth/login" in page.url:
            return "out"
        for marker in LOGOUT_MARKERS:
            if await page.locator(marker).count() > 0:
                return "out"
        for marker in COMPOSER_MARKERS:
            if await page.locator(marker).count() > 0:
                return "in"
        link = page.get_by_role("link", name=re.compile(r"log ?in", re.I))
        if await link.count() > 0:
            return "out"
    except Exception:
        return "unknown"
    return "unknown"


async def voice_rate_limited(page) -> bool:
    """True when the voice rate-limit modal is on the page."""
    try:
        for selector in VOICE_RATE_LIMIT_SELECTORS:
            if await page.locator(selector).count() > 0:
                return True
        return False
    except Exception:
        return False


async def voice_overlay_present(page) -> bool:
    """Best-effort check that a voice session overlay is on the page.

    Only two signals count, both verified against the live site:
      - an audio element driven by a MediaStream and playing,
      - the live orb container (exact testid; the entry page also contains
        voice-branded marketing elements that must not match).
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
        return await page.locator(
            'div[data-testid="avatar-overlay-voice-orb"]'
        ).count() > 0
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

    async def start(self) -> bool:
        page = self.page
        if time.monotonic() < self.limit_until:
            # Quiet period after the rate-limit modal; do not hammer.
            return False
        if await voice_overlay_present(page):
            # The page can open straight into a live voice session (for
            # example ?mode=voice). Nothing to click.
            self.active = True
            self.failures = 0
            log.info("voice session already active on the page")
            return True
        if await voice_rate_limited(page):
            await self._dismiss_rate_limit(page)
            self.limit_hit = True
            self.limit_until = time.monotonic() + RATE_LIMIT_BACKOFF_S
            STATUS.update(
                voice_limited=True,
                last_error="voice rate limited (anonymous free tier); "
                           "log in via noVNC or wait for the reset",
            )
            log.warning(
                "voice rate-limited; backing off for %ss",
                RATE_LIMIT_BACKOFF_S,
            )
            return False
        button = await find_voice_button(page)
        if button is None:
            if self.failures == 0 or self.failures % 10 == 0:
                await save_debug(page, "voice-button-missing")
            self.failures += 1
            return False
        if not await click(button):
            self.failures += 1
            await save_debug(page, "voice-click-failed")
            return False

        deadline = time.monotonic() + CFG.voice_start_timeout
        while time.monotonic() < deadline:
            if await voice_overlay_present(page):
                self.active = True
                self.failures = 0
                self.limit_hit = False
                STATUS.update(voice_limited=False)
                log.info("voice session started")
                return True
            if await voice_rate_limited(page):
                break
            await asyncio.sleep(0.5)

        self.failures += 1
        await save_debug(page, "voice-start-timeout")
        log.warning("voice session did not start within %ss", CFG.voice_start_timeout)
        return False

    async def _dismiss_rate_limit(self, page) -> None:
        try:
            close = page.locator(
                '[data-testid="modal-voice-rate-limit"] [data-testid="close-button"]'
            )
            if await close.count() > 0 and await close.first.is_visible():
                await click(close.first)
                return
            await page.keyboard.press("Escape")
        except Exception:
            pass

    async def end(self) -> bool:
        page = self.page
        control = await find_end_control(page)
        if control is not None:
            await click(control)
        else:
            # Last resort: the overlay also closes on Escape.
            await page.keyboard.press("Escape")

        deadline = time.monotonic() + CFG.voice_end_timeout
        while time.monotonic() < deadline:
            if not await voice_overlay_present(page):
                self.active = False
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

    await page.goto(CFG.url, wait_until="domcontentloaded", timeout=60_000)
    log.info("opened %s", CFG.url)

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
                if voice.active or await voice_overlay_present(page):
                    await voice.recover()
                if voice.active:
                    # No call can be active; reset to a fresh chat for the
                    # next one.
                    try:
                        await page.goto(CFG.url, wait_until="domcontentloaded", timeout=60_000)
                        voice.active = False
                        STATUS.update(voice_active=False)
                        log.info("bridge unreachable; fresh chat opened")
                    except Exception as exc:
                        log.warning("could not open a fresh chat: %s", exc)
                await asyncio.sleep(CFG.poll_interval)
                continue

            if not active and not voice.active and await voice_overlay_present(page):
                # A stray overlay (page reloaded into voice mode, or the
                # session leaked): close it so the allowance is not burned
                # between calls. If it will not close, reload once instead
                # of thrashing the page every poll.
                log.info("closing stray voice overlay between calls")
                await voice.recover()
                if await voice_overlay_present(page):
                    try:
                        await page.goto(CFG.url, wait_until="domcontentloaded", timeout=60_000)
                        log.info("stray overlay would not close; reloaded the page")
                    except Exception as exc:
                        log.warning("could not reload after failed cleanup: %s", exc)

            if active and not voice.active:
                if CFG.require_login and login_state != "in":
                    log.warning("call active but not logged in; cannot start voice")
                    if failures == 0:
                        await save_debug(page, "call-without-login")
                    failures += 1
                elif await voice.start():
                    sessions = STATUS.snapshot()["voice_sessions_started"] + 1
                    STATUS.update(
                        voice_active=True,
                        ready=True,
                        voice_sessions_started=sessions,
                    )
                elif not voice.limit_hit:
                    failures += 1

            elif not active and voice.active:
                ended = await voice.end()
                if ended:
                    sessions = STATUS.snapshot()["voice_sessions_ended"] + 1
                    STATUS.update(voice_sessions_ended=sessions)
                else:
                    failures += 1
                # Open a fresh chat so the next call starts a new
                # conversation (and as a hard cleanup: navigating away
                # tears down a voice overlay that refused to close).
                try:
                    await page.goto(CFG.url, wait_until="domcontentloaded", timeout=60_000)
                    voice.active = False
                    STATUS.update(voice_active=False)
                    log.info("fresh chat opened for the next call")
                except Exception as exc:
                    failures += 1
                    log.warning("could not open a fresh chat: %s", exc)

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
        "controller starting: url=%s bridge=%s login_only=%s profile=%s",
        CFG.url,
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
