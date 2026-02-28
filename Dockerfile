# Multi-stage build - compiles Go inside Docker
FROM golang:1.22-alpine AS builder
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o sfu-test .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/sfu-test .
COPY --from=builder /app/index.html .
EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q --spider http://localhost:8081/health || exit 1
CMD ["./sfu-test"]
