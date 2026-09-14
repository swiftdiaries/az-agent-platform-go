FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/agent-platform ./cmd/agent-platform

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/agent-platform /app/agent-platform
COPY configs /app/configs
USER nonroot:nonroot
ENTRYPOINT ["/app/agent-platform"]
