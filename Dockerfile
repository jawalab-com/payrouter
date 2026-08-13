FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/payrouter ./cmd/payrouter

FROM alpine:3.22
RUN addgroup -S payrouter && adduser -S -G payrouter payrouter
COPY --from=builder /out/payrouter /usr/local/bin/payrouter
# The orchestrator fee schedule must be in the image: without it the router falls
# back to the compiled-in defaults and silently ignores every override in this
# file. Mount your own over /etc/payrouter/config.yaml to customize rates.
COPY --from=builder /src/config.yaml /etc/payrouter/config.yaml
ENV PAYMENT_CONFIG_PATH=/etc/payrouter/config.yaml
USER payrouter
EXPOSE 8787
ENTRYPOINT ["payrouter"]
