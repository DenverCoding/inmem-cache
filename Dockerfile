FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /out/scopecache ./cmd/scopecache

FROM alpine:latest
RUN apk add --no-cache curl
COPY --from=builder /out/scopecache /usr/bin/scopecache
CMD ["/usr/bin/scopecache"]
