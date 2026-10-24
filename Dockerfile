# syntax=docker/dockerfile:1
# Static hermes-go on distroless. The image has no shell, so the local
# `terminal` tool cannot run commands inside it; use the Docker terminal
# backend or a base image with a shell for that.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/hermes-go ./cmd/hermes-go
# distroless has no mkdir; create the home here so a named volume inherits
# the nonroot owner instead of root.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hermes-go /usr/local/bin/hermes-go
COPY --from=build --chown=65532:65532 /out/data /data
# The Hermes home (config.yaml, .env, state.db, skills, cron) lives in /data.
ENV HERMES_HOME=/data
VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/hermes-go"]
CMD ["--help"]
