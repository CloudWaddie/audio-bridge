FROM alpine:3.20

# The pulseaudio package already creates a "pulse" user, and Alpine ships an
# "audio" group, so pick a name that collides with neither. The UID matches the
# bridge user (10001) so both containers can share the /pulse volume.
RUN apk add --no-cache pulseaudio pulseaudio-utils tini python3 \
    && addgroup -g 10001 audiosocket \
    && adduser -D -u 10001 -G audiosocket -s /sbin/nologin audiosocket \
    && mkdir -p /pulse \
    && chown audiosocket:audiosocket /pulse

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

USER audiosocket
ENV HOME=/home/audiosocket \
    XDG_RUNTIME_DIR=/tmp/pulse-runtime
VOLUME ["/pulse"]

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
