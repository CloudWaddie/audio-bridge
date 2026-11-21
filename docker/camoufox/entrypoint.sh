#!/bin/sh
# Starts the virtual display, optional VNC (used for the one-time ChatGPT
# login through noVNC) and hands over to the controller.
set -eu

DISPLAY_NUM=":99"
SCREEN="${CF_SCREEN:-1920x1080x24}"

Xvfb "${DISPLAY_NUM}" -screen 0 "${SCREEN}" -nolisten tcp -ac &

# Let the X server come up before x11vnc attaches to it.
sleep 1

if [ "${CF_VNC_ENABLED:-true}" = "true" ]; then
    if [ -n "${CF_VNC_PASSWORD:-}" ]; then
        x11vnc -storepasswd "${CF_VNC_PASSWORD}" /tmp/vnc.passwd
        x11vnc -display "${DISPLAY_NUM}" -forever -shared -quiet -bg \
            -rfbauth /tmp/vnc.passwd
    else
        # No password. Acceptable only because port 6080 is never published
        # to the host by default; see docs/chatgpt-voice.md.
        x11vnc -display "${DISPLAY_NUM}" -forever -shared -quiet -bg -nopw
    fi
    websockify --web=/usr/share/novnc 6080 localhost:5900 &
fi

exec python3 /app/controller.py
