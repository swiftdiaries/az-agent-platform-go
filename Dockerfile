FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/agent-platform ./cmd/agent-platform

# Per-agent bundle image (docker build --target bundle). Holds journeys.yaml,
# conversation.md and skills/ for `agent-platform activate --bundle`.
FROM busybox:1.36.1 AS bundle
COPY configs /bundle

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/agent-platform /app/agent-platform
USER nonroot:nonroot
ENTRYPOINT ["/app/agent-platform"]
