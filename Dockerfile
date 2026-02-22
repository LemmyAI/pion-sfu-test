FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o sfu-test .

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/sfu-test .
COPY --from=builder /app/index.html .
EXPOSE 8080
CMD ["./sfu-test"]