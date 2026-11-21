# syntax=docker/dockerfile:1
FROM debian:bookworm-slim

ARG CAMOUFOX_VERSION=0.5.9

# xvfb            : virtual display the headed browser runs on (display :99)
# x11vnc, novnc,  : one-time interactive login over VNC/noVNC
# websockify
# libgtk-3-0 ...  : Firefox runtime libraries (list from the Camoufox CI image)
# libpulse0      : Firefox audio backend (playback + getUserMedia capture)
# pulseaudio-utils: pactl/parec for audio diagnostics inside the container
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates tini \
        xvfb x11vnc novnc websockify \
        libgtk-3-0 libasound2 libdbus-glib-1-2 libx11-xcb1 libxcb-shm0 \
        libxcomposite1 libxcursor1 libxdamage1 libxfixes3 libxi6 \
        libxrandr2 libxtst6 libpci3 \
        libpulse0 pulseaudio-utils \
        fonts-liberation \
        python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

# Same UID as the bridge and audio containers (10001): all three share the
# /pulse socket volume. useradd/groupadd, not adduser: Debian's adduser
# rejects --gid in --system mode.
RUN groupadd -g 10001 audiosocket \
    && useradd --system --uid 10001 --gid 10001 --create-home \
        --home-dir /home/camoufox --shell /bin/bash camoufox \
    && mkdir -p /data \
    && chown -R camoufox:audiosocket /data /home/camoufox

COPY requirements.txt /tmp/requirements.txt
RUN pip3 install --no-cache-dir --break-system-packages -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt

# fpgen stores its model inside the installed package directory. pip
# installed it as root, so hand just that package to the runtime user; the
# browser cache already lives in the camoufox home. pip show locates the
# package without importing it: importing fpgen has the side effect of
# starting a model download.
RUN FPGEN_DIR="$(python3 -m pip show fpgen | sed -n 's/^Location: //p')/fpgen" \
    && test -d "$FPGEN_DIR" \
    && chown -R camoufox:audiosocket "$FPGEN_DIR"

# Download the pinned browser build at image build time. Must run as the user
# that owns the cache, which is also the user the browser runs as.
USER camoufox
RUN camoufox fetch

COPY --chown=camoufox:audiosocket entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --chown=camoufox:audiosocket controller.py /app/controller.py
RUN chmod 0755 /usr/local/bin/entrypoint.sh

ENV HOME=/home/camoufox \
    PYTHONUNBUFFERED=1 \
    CF_PROFILE_DIR=/data/profile \
    CF_SCREEN=1920x1080x24

WORKDIR /app
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=3)" || exit 1

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
