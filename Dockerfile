# Local build: docker build -t tether .
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/adeelahmad/tether/internal/version.Version=${VERSION}" \
    -o /out/tether ./cmd/tether

FROM alpine:3.22
RUN apk add --no-cache chromium ca-certificates font-noto tini \
 && adduser -D -h /home/tether tether \
 && mkdir -p /data && chown tether:tether /data
COPY --from=build /out/tether /usr/local/bin/tether
USER tether
ENV TETHER_DATA_DIR=/data \
    TETHER_CHROME_ARGS="--no-sandbox --disable-dev-shm-usage"
VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["/sbin/tini", "--", "tether"]
CMD ["serve", "--launch", "--headless", "--addr", "0.0.0.0:8787"]
