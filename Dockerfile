FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /out/inmem-cache .

FROM alpine:latest
RUN apk add --no-cache curl
COPY --from=builder /out/inmem-cache /usr/bin/inmem-cache
CMD ["/usr/bin/inmem-cache"]
