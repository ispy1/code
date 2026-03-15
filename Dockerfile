# Builder stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o spot-injector .

# Final stage
FROM scratch

COPY --from=builder /app/spot-injector /spot-injector
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8443

USER 65532:65532

ENTRYPOINT ["/spot-injector"]
