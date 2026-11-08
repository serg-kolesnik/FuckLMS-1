# --- STEP 1: Builder ---
FROM golang:1.27.3-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Точка, а не main.go: пакет main раскидан по нескольким файлам
# (config.go, store.go, api.go, mark.go, lms.go), одиночный файл не собирается.
RUN go build -o server .

# --- STEP 2: Production ---
FROM alpine:3.24.4
WORKDIR /app

# CA-сертификаты обязательны: сервис ходит в Moodle по HTTPS,
# в базовом alpine-образе их нет.
RUN apk add --no-cache ca-certificates

COPY --from=builder /app/server .

CMD ["./server"]
