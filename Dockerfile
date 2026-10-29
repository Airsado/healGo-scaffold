FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server .

FROM alpine:3.23@sha256:ff7f0c727dd796aa36787bfcd9a64bf35a6ce72a73451b3359fde849b6703d63
RUN apk add --no-cache ca-certificates && addgroup -g 10001 app && adduser -D -H -u 10001 -G app app
WORKDIR /app
COPY --from=builder --chown=10001:10001 /out/server /app/server
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/server"]
CMD ["serve", "-config", "/app/configs/config.yaml"]
