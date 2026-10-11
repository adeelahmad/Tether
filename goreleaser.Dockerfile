# Release image: the prebuilt binary plus Chromium, so `--launch --headless`
# works out of the box. To drive a Chrome running elsewhere, pass
# --chrome http://host:9222 instead of --launch.
FROM alpine:3.24
RUN apk add --no-cache chromium ca-certificates font-noto tini \
 && adduser -D -h /home/tether tether \
 && mkdir -p /data && chown tether:tether /data
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/tether /usr/local/bin/tether
USER tether
ENV TETHER_DATA_DIR=/data \
    TETHER_CHROME_ARGS="--no-sandbox --disable-dev-shm-usage"
VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["/sbin/tini", "--", "tether"]
# Listening beyond loopback requires TETHER_TOKEN (or --insecure-allow-public).
CMD ["serve", "--launch", "--headless", "--addr", "0.0.0.0:8787"]
