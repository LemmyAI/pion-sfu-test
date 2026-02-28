# Pre-built binary approach - build locally first: go build -o sfu-test .
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY sfu-test .
COPY index.html .
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q --spider http://localhost:8080/health || exit 1
CMD ["./sfu-test"]
