#!/usr/bin/env python3
"""Probe the real chatgpt.com DOM.

Dev tool: runs headed Camoufox (or headless with CF_HEADLESS=true), injects
exported session cookies when available (CF_COOKIES), opens the target page
and exercises the same selectors and the same start/end/fresh-chat flow the
production controller uses, printing everything it finds. Results feed back
into docker/camoufox/controller.py.

Usage:
    python scripts/probe_chatgpt.py                       # anonymous voice page
    CF_COOKIES=... CF_URL=https://chatgpt.com/ python scripts/probe_chatgpt.py

Artifacts land in scripts/probe-debug/ (screenshots + HTML). The persistent
profile lives in .probe-profile so a session survives between runs.
"""

from __future__ import annotations

import asyncio
import os
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "docker" / "camoufox"))

DEBUG = ROOT / "scripts" / "probe-debug"
# controller.py reads the environment at import time; pin its debug output
# into the probe folder before importing it.
os.environ.setdefault("CF_DEBUG_DIR", str(DEBUG))

import controller as ctl  # noqa: E402  (production selectors and helpers)

from camoufox.async_api import AsyncCamoufox  # noqa: E402

PROFILE = ROOT / ".probe-profile"
DEBUG = ROOT / "scripts" / "probe-debug"
URL = os.environ.get("CF_URL", "https://chatgpt.com/?mode=voice")
COOKIES = os.environ.get("CF_COOKIES", "/tmp/creds/cookies.json")
REQUIRE_LOGIN = os.environ.get("CF_REQUIRE_LOGIN", "").lower() in ("1", "true")

PROBE_PREFS = dict(ctl.FIREFOX_PREFS)


def step(name: str, ok: bool, detail: str = "") -> None:
    mark = "PASS" if ok else "FAIL"
    print(f"[{mark}] {name}" + (f" :: {detail}" if detail else ""), flush=True)


def load_playwright_cookies(path: str) -> list[dict]:
    """Convert a Cookie-Editor JSON export into Playwright cookie dicts."""
    import json

    with open(path, encoding="utf-8") as handle:
        raw = json.load(handle)
    if isinstance(raw, dict):
        raw = raw.get("cookies", [])

    same_site_map = {
        "strict": "Strict",
        "lax": "Lax",
        "no_restriction": "None",
        "none": "None",
    }
    out = []
    for cookie in raw:
        same_site = same_site_map.get(
            str(cookie.get("sameSite", "")).lower(), "Lax"
        )
        converted = {
            "name": cookie["name"],
            "value": cookie["value"],
            "domain": cookie.get("domain", ".chatgpt.com"),
            "path": cookie.get("path", "/"),
            "secure": bool(cookie.get("secure", True)),
            "httpOnly": bool(cookie.get("httpOnly", False)),
            "sameSite": same_site,
        }
        if cookie.get("expirationDate"):
            converted["expires"] = float(cookie["expirationDate"])
        if cookie.get("hostOnly"):
            converted["domain"] = converted["domain"].lstrip(".")
        out.append(converted)
    return out


async def dump_buttons(page, pattern: str = "") -> None:
    """Log every visible button with its identity attributes."""
    info = await page.evaluate(
        """(pattern) => {
            const rows = [];
            for (const b of document.querySelectorAll('button')) {
                if (!(b.offsetWidth || b.offsetHeight)) continue;
                const label = b.getAttribute('aria-label') || '';
                const tid = b.getAttribute('data-testid') || '';
                const text = (b.textContent || '').trim().slice(0, 40);
                if (pattern && !new RegExp(pattern, 'i').test(label + ' ' + tid + ' ' + text)) {
                    continue;
                }
                rows.push({
                    tid: tid || null,
                    label: label || null,
                    text: text || null,
                });
            }
            return rows;
        }""",
        pattern,
    )
    for row in info:
        print(f"    button: {row}", flush=True)
    if not info:
        print("    button: <none matched>", flush=True)


async def dump_overlay_dom(page) -> None:
    """Capture every visible control while the voice overlay is up."""
    info = await page.evaluate(
        """() => {
            const rows = [];
            for (const el of document.querySelectorAll(
                    '[data-testid*="voice" i],[data-testid*="call" i],[id*="voice" i]')) {
                rows.push({
                    tag: el.tagName,
                    tid: el.getAttribute('data-testid') || el.id,
                    label: el.getAttribute('aria-label') || null,
                    text: (el.textContent || '').trim().slice(0, 40) || null,
                });
            }
            return rows;
        }"""
    )
    for row in info:
        print(f"    overlay-element: {row}", flush=True)
    await dump_buttons(page)


async def overlay_signal(page) -> str | None:
    """Which overlay signal is currently true."""
    playing = await page.evaluate(
        """() => {
            for (const a of document.querySelectorAll('audio')) {
                if (a.srcObject && !a.paused) return true;
            }
            return false;
        }"""
    )
    if playing:
        return "audio-playing"
    if await page.locator('div[data-testid*="voice" i]').count() > 0:
        return "voice-container"
    if await page.locator('button[aria-label*="mute" i]').count() > 0:
        return "mute-control"
    return None


async def main() -> None:
    DEBUG.mkdir(parents=True, exist_ok=True)
    print(f"profile: {PROFILE}", flush=True)
    print(f"url: {URL}", flush=True)

    async with AsyncCamoufox(
        persistent_context=True,
        user_data_dir=str(PROFILE),
        headless=os.environ.get("CF_HEADLESS", "").lower() in ("1", "true"),
        os="windows",             # match the workstation; fewer spoof artifacts
        block_webrtc=False,
        disable_coop=True,
        firefox_user_prefs=PROBE_PREFS,
    ) as context:
        page = context.pages[0] if context.pages else await context.new_page()

        # Inject an exported ChatGPT session when available. Values never
        # leave this process.
        if COOKIES and os.path.isfile(COOKIES):
            cookies = load_playwright_cookies(COOKIES)
            await context.add_cookies(cookies)
            print(f"injected {len(cookies)} cookies from {COOKIES}", flush=True)

        await page.goto(URL, wait_until="domcontentloaded", timeout=60_000)
        await asyncio.sleep(5)  # let the SPA settle

        # ---- 1. where did we land --------------------------------------------
        print(f"landed on: {page.url}", flush=True)
        await ctl.save_debug(page, "probe-initial")
        state = await ctl.detect_login(page)
        step("login-state", True, f"detect_login={state}")
        if REQUIRE_LOGIN and state != "in":
            print("waiting for login inside the Firefox window...", flush=True)
            deadline = time.monotonic() + 15 * 60
            while state != "in" and time.monotonic() < deadline:
                await asyncio.sleep(2)
                state = await ctl.detect_login(page)
            step("login", state == "in", f"detect_login={state}")
            if state != "in":
                return
        await asyncio.sleep(3)

        # ---- 2. map the page --------------------------------------------------
        print("--- voice/dictate/mic buttons:", flush=True)
        await dump_buttons(page, r"voice|dictat|mic|call|talk")
        print("--- all visible buttons (first 40):", flush=True)
        await dump_buttons(page, "")
        composer = await page.evaluate(
            """() => {
                const el = document.querySelector('#prompt-textarea')
                        || document.querySelector('[id*="prompt" i]')
                        || document.querySelector('textarea');
                return el ? {id: el.id, tag: el.tagName} : null;
            }"""
        )
        step("composer", composer is not None, str(composer))
        await ctl.save_debug(page, "probe-mapped")

        # ---- 3. find the voice button with production selectors --------------
        if await ctl.voice_overlay_present(page):
            step("voice-already-active", True, "overlay present on load")
        else:
            button = await ctl.find_voice_button(page)
            step("voice-button", button is not None,
                 "" if button is not None else "not found via production selectors")

            # ---- 4. click it, wait for the overlay --------------------------
            if button is None:
                print("no voice button found; inspect probe-debug artifacts",
                      flush=True)
                return
            clicked = await ctl.click(button)
            step("voice-click", clicked)

        deadline = time.monotonic() + 20
        signal = None
        while time.monotonic() < deadline:
            if await ctl.voice_overlay_present(page):
                signal = await overlay_signal(page)
                break
            await asyncio.sleep(0.5)
        step("voice-overlay", signal is not None, f"signal={signal}")
        await ctl.save_debug(page, "probe-voice-active")
        await dump_overlay_dom(page)

        if signal is None:
            print("voice overlay did not come up; it may need a mic, "
                  "or different selectors", flush=True)
            return

        # ---- 5. let the greeting play, then end the session ------------------
        print("voice session live (the greeting should be audible); "
              "ending it in 10 s...", flush=True)
        await asyncio.sleep(10)
        control = await ctl.find_end_control(page)
        step("end-control", control is not None)
        if control is not None:
            await ctl.click(control)
        else:
            await page.keyboard.press("Escape")
        gone = False
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if not await ctl.voice_overlay_present(page):
                gone = True
                break
            await asyncio.sleep(0.5)
        step("voice-ended", gone)
        await ctl.save_debug(page, "probe-voice-ended")

        # ---- 6. fresh page: does voice auto-resume after reload? ---------------
        try:
            await page.goto(URL, wait_until="domcontentloaded", timeout=60_000)
            await asyncio.sleep(5)
            state = await ctl.detect_login(page)
            resumed = await ctl.voice_overlay_present(page)
            if resumed:
                resumed_signal = await overlay_signal(page)
                await ctl.save_debug(page, "probe-resumed")
                await dump_buttons(page, r"end|mute|close|call")
                # close it again so the session is not left running
                control = await ctl.find_end_control(page)
                if control is not None:
                    await ctl.click(control)
                else:
                    await page.keyboard.press("Escape")
                await asyncio.sleep(2)
            else:
                resumed_signal = None
                await ctl.save_debug(page, "probe-reloaded-idle")
                await dump_buttons(page, r"voice|talk|mic")
            step("fresh-page", True,
                 f"detect_login={state} overlay_after_reload={resumed} "
                 f"signal={resumed_signal}")
        except Exception as exc:
            step("fresh-page", False, str(exc))

    print("probe finished", flush=True)


if __name__ == "__main__":
    asyncio.run(main())
