FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ledgerserver ./cmd/ledgerserver && \
    CGO_ENABLED=0 go build -trimpath -o /out/ledgerctl ./cmd/ledgerctl

FROM alpine:3.23
RUN addgroup -S ledger && adduser -S -G ledger ledger && mkdir /data && chown ledger:ledger /data
COPY --from=build /out/* /usr/local/bin/
USER ledger
WORKDIR /data
EXPOSE 50051 8081
ENTRYPOINT ["ledgerserver"]
