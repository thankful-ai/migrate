FROM --platform=$BUILDPLATFORM golang:1.21 AS build

WORKDIR /go/src/app
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 \
  GOOS=${TARGETOS} \
  GOARCH=${TARGETARCH} \
  GOCACHE=/cache/go \
  GOMODCACHE=/cache/gomod

RUN <<-EOF
  go env -w GOCACHE=${GOCACHE}
  go env -w GOMODCACHE=${GOMODCACHE}
EOF

RUN --mount=type=bind,source=go.mod,target=/go/src/app/go.mod,readonly \
  --mount=type=bind,source=go.sum,target=/go/src/app/go.sum,readonly \
  --mount=type=cache,target=${GOCACHE} \
  --mount=type=cache,target=${GOMODCACHE} \
  --mount=type=cache,target=/go/pkg \
  go mod download -x

RUN --mount=type=bind,source=.,target=/go/src/app,readonly \
  --mount=type=cache,target=${GOCACHE} \
  --mount=type=cache,target=${GOMODCACHE} \
  --mount=type=cache,target=/go/pkg \
  go build -x -a -ldflags="-w -s" -trimpath -o /go/bin/app ./cmd/migrate

FROM alpine:3.18 AS main
RUN apk add --no-cache ca-certificates
USER nobody
COPY scripts/docker-entrypoint.sh /docker-entrypoint.sh
COPY --from=build /go/bin/app /migrate
ENTRYPOINT ["/docker-entrypoint.sh"]
