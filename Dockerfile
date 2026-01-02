# build Go gateway
FROM golang:1.25-alpine AS go-builder

WORKDIR /app/gateway
COPY gateway/go.mod gateway/go.sum ./
RUN go mod download

COPY gateway/ ./
RUN go build -o ig-gateway ./cmd/ig-gateway

# python runtime
FROM python:3.13-slim

WORKDIR /app

# copy Go binary from builder
COPY --from=go-builder /app/gateway/ig-gateway /app/gateway/ig-gateway

# install Python dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# copy Python source
COPY src/ ./src/

# expose port
EXPOSE 8000

# run the server
CMD ["python", "src/server.py"]
