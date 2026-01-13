FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY main.go .
RUN go mod init sample-app && \
    go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build -o app main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/app .
EXPOSE 8080 6060
CMD ["./app"]

