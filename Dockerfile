# --- Build stage: compile the static Linux binary ---
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tgarchive .

# --- Runtime stage: minimal image with just the binary ---
FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/tgarchive /app/tgarchive

# SQLite lives here - mount a volume to persist it across container upgrades.
RUN mkdir -p /data
ENV DB_PATH=/data/tgarchive.db \
    LISTEN_ADDR=:8080

EXPOSE 8080
ENTRYPOINT ["/app/tgarchive"]
