# Build once, run as either `serve` or `work` — the command is the only
# difference between the API and the worker.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Copy manifests first so dependency download caches across source changes.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# CGO off produces a static binary that runs on a minimal base image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/velora ./cmd/velora

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 velora
USER velora
WORKDIR /app
COPY --from=build /out/velora /app/velora

EXPOSE 8080
ENTRYPOINT ["/app/velora"]
CMD ["serve"]
