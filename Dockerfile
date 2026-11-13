# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY system_prompt_chat.txt system_prompt_muse.txt base_tools.json base_tools_muse.json ./
COPY public/ ./public/

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
