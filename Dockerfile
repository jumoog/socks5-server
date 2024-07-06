FROM golang AS builder
WORKDIR /go/src/github.com/jummog/socks5
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-s' -o ./socks5

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /go/src/github.com/jummog/socks5/socks5 /
ENTRYPOINT ["/socks5"]
