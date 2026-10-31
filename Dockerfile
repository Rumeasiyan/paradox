# PARADOX image. Build: docker build -t paradox .
# Runs as a non-root user on a read-only root file system; the binary
# writes nothing to disk (logs go to stderr).

FROM golang:1.27.3-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off
COPY go.mod go.sum ./
COPY sign/go.mod sign/
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/paradox ./cmd/paradox

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="PARADOX" \
      org.opencontainers.image.description="Scheduler and job queue for systems that sleep when idle" \
      org.opencontainers.image.source="https://github.com/Rumeasiyan/paradox" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/paradox /paradox
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD ["/paradox", "-healthcheck"]
ENTRYPOINT ["/paradox"]
CMD ["serve"]
