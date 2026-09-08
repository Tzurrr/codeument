FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /codeument ./cmd/codeument

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -h /var/lib/codeument codeument
COPY --from=build /codeument /usr/local/bin/codeument
USER codeument
VOLUME ["/var/lib/codeument", "/etc/codeument"]
EXPOSE 8443
ENTRYPOINT ["codeument", "relay", "serve", "--relay-config", "/etc/codeument/relay.yaml"]
