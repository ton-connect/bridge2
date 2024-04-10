FROM golang:1.22 AS gobuild
WORKDIR /build-dir
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN go build -v -o /tmp/api ./cmd/bridge

FROM ubuntu:22.04 as api
RUN mkdir -p /app/lib
RUN apt-get update && \
    apt-get install -y openssl ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=gobuild /tmp/api /app/api
CMD ["/app/api"]
