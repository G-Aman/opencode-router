# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY *.go ./

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o opencode-router .

# Minimal alpine runtime
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/opencode-router /app/
COPY public/ /app/public/
COPY assets/ /app/assets/
COPY base_tools.json base_tools_muse.json /app/

EXPOSE 8787

ENTRYPOINT ["/app/opencode-router"]
CMD ["8787"]
