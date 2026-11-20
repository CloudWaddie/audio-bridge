FROM alpine:3.20

# The pulseaudio package already creates a "pulse" user, so use a dedicated
# "audio" user with the same UID as the bridge user (10001). Both containers
# then own the shared /pulse volume without extra permission juggling.
RUN apk add --no-cache pulseaudio pulseaudio-utils tini \
    && adduser -D -u 10001 -s /sbin/nologin audio \
    && mkdir -p /pulse \
    && chown audio:audio /pulse

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

USER audio
ENV HOME=/home/audio \
    XDG_RUNTIME_DIR=/tmp/pulse-runtime
VOLUME ["/pulse"]

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
