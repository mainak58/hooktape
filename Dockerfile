# Builds on the host's platform and cross-compiles, so a multi-platform build
# needs no emulation. Pure Go, so the final image holds only the binary.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=none
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/mainak58/hooktape/internal/cli.version=$VERSION -X github.com/mainak58/hooktape/internal/cli.commit=$COMMIT" \
    -o /hooktape ./cmd/hooktape
RUN mkdir /data

# distroless ships CA certificates for https forwards and runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hooktape /usr/local/bin/hooktape
COPY --from=build --chown=nonroot:nonroot /data /data
ENV HOOKTAPE_DB=/data/hooktape.db
VOLUME /data
EXPOSE 8080 8081

# Inside a container 127.0.0.1 can't be reached from outside, so bind every
# interface; `docker run -p` decides what is actually exposed. Arguments to
# `docker run` are appended, e.g. --forward http://host.docker.internal:3000
ENTRYPOINT ["hooktape", "up", "--addr", "0.0.0.0", "--ui-addr", "0.0.0.0", "--no-tunnel"]
