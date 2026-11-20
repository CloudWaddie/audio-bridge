FROM alpine:3.20

RUN apk add --no-cache pulseaudio pulseaudio-utils tini \
    && adduser -D -H -u 10001 -s /sbin/nologin pulse \
    && mkdir -p /pulse \
    && chown pulse:pulse /pulse

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

USER pulse
ENV XDG_RUNTIME_DIR=/tmp/pulse-runtime
VOLUME ["/pulse"]

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
