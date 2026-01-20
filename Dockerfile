# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.mod
COPY go.sum go.sum

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/ pkg/

# Build (TARGETARCH is set by buildx for multi-platform builds)
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -a -o manager main.go

# Runtime stage with pg_dump
FROM alpine:3.20

# Install PostgreSQL client (pg_dump only, minimal)
RUN apk add --no-cache postgresql16-client \
    && rm -rf /var/cache/apk/*

WORKDIR /

COPY --from=builder /workspace/manager .

# Run as non-root
RUN adduser -D -u 65532 nonroot
USER 65532:65532

ENTRYPOINT ["/manager"]
