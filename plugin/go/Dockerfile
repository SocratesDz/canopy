# Dockerfile for Canopy + Go Plugin (canoLiq)
# Build from repository root: docker build -f plugin/go/Dockerfile -t canopy-canoliq .

# Stage 1: Build Canopy (with explorer/wallet assets)
FROM golang:1.25-alpine AS canopy-builder
RUN apk add --no-cache make bash nodejs npm
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build/wallet
RUN make build/explorer
RUN go build -trimpath -ldflags="-s -w" -o /canopy ./cmd/main/...

# Stage 2: Build Go Plugin (canoLiq mode)
FROM golang:1.25-alpine AS plugin-builder
WORKDIR /app
COPY plugin/go/ ./
RUN go build -o /plugin .

# Stage 3: Runtime
FROM alpine:latest
WORKDIR /app
RUN apk add --no-cache bash procps && mkdir -p /tmp/plugin /root/.canopy plugin/go

COPY --from=canopy-builder /canopy .
COPY --from=plugin-builder /plugin plugin/go/go-plugin
COPY plugin/go/pluginctl.sh plugin/go/pluginctl.sh
COPY plugin/go/canoliq/canoliq-config.localnet.json plugin/go/canoliq/canoliq-config.localnet.json
COPY plugin/go/canoliq/canoliq-config.testnet.json plugin/go/canoliq/canoliq-config.testnet.json
COPY plugin/go/canoliq/genesis.localnet.json plugin/go/canoliq/genesis.localnet.json
COPY plugin/go/canoliq/genesis.testnet.json plugin/go/canoliq/genesis.testnet.json
RUN chmod +x plugin/go/pluginctl.sh plugin/go/go-plugin

# Entrypoint: start canopy node then canoLiq plugin once socket is ready
COPY plugin/go/start.sh /app/start.sh
RUN chmod +x /app/start.sh

CMD ["/app/start.sh"]
