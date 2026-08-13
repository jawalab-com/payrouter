FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/payment-facade ./cmd/facade

FROM alpine:3.22
RUN addgroup -S facade && adduser -S -G facade facade
COPY --from=builder /out/payment-facade /usr/local/bin/payment-facade
USER facade
EXPOSE 8787
ENTRYPOINT ["payment-facade"]
