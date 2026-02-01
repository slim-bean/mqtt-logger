# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum* ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go .

# Build the binary (ARCH will be set by buildx for multi-arch builds)
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o mqtt-logger -ldflags '-w -s' main.go

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /mqtt-logger

# Copy binary from builder
COPY --from=builder /build/mqtt-logger .

# Set timezone (can be overridden via TZ env var)
ENV TZ=America/New_York

ENTRYPOINT ["./mqtt-logger"]
