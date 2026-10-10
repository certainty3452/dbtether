FROM golang:1.26-alpine AS builder

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/ pkg/

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -a -o manager main.go

FROM alpine:3.23

RUN apk upgrade --no-cache \
    && apk add --no-cache postgresql16-client postgresql17-client postgresql18-client \
    && rm -rf /var/cache/apk/*

WORKDIR /

COPY --from=builder /workspace/manager .

RUN adduser -D -u 65532 nonroot
USER 65532:65532

ENTRYPOINT ["/manager"]
