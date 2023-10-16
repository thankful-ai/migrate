FROM --platform=$BUILDPLATFORM golang:1.21.3 AS build

WORKDIR /go/src/app
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 \
  GOOS=${TARGETOS} \
  GOARCH=${TARGETARCH} \
  GOCACHE=/cache/go \
  GOMODCACHE=/cache/gomod

RUN <<EOR
go env -w GOCACHE=${GOCACHE}
go env -w GOMODCACHE=${GOMODCACHE}
EOR

RUN --mount=type=bind,source=go.mod,target=/go/src/app/go.mod,readonly \
  --mount=type=bind,source=go.sum,target=/go/src/app/go.sum,readonly \
  --mount=type=cache,target=${GOCACHE} \
  --mount=type=cache,target=${GOMODCACHE} \
  --mount=type=cache,target=/go/pkg \
  go mod download -x -json

RUN --mount=type=bind,source=.,target=/go/src/app,readonly \
  --mount=type=cache,target=${GOCACHE} \
  --mount=type=cache,target=${GOMODCACHE} \
  --mount=type=cache,target=/go/pkg \
  go build -a -ldflags="-w -s" -o /go/bin/app ./cmd/migrate

FROM gcr.io/distroless/static-debian12 AS main
COPY --from=build /go/bin/app /migrate
ENTRYPOINT ["/migrate"]
