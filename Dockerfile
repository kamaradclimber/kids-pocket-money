# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kids-pocket-money .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 65532 app \
    && adduser -D -u 65532 -G app -h /data app \
    && mkdir -p /data \
    && chown -R app:app /data

COPY --from=build /out/kids-pocket-money /usr/local/bin/kids-pocket-money

USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/kids-pocket-money"]