# syntax=docker/dockerfile:1

FROM node:24-alpine AS frontend
WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web ./web
RUN mkdir -p internal/unifiedui/static \
    && cd web \
    && npm run build

FROM golang:1.25-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
COPY allocation-service/go.mod allocation-service/go.sum ./allocation-service/
RUN go mod download
COPY . .
COPY --from=frontend /src/internal/unifiedui/static ./internal/unifiedui/static
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vitals ./cmd/vitals \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vitals-migrate ./cmd/vitals-migrate \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vitals-password-hash ./docker/hash-password.go

FROM alpine:3.22
RUN apk add --no-cache ca-certificates su-exec \
    && addgroup -S -g 10001 vitals \
    && adduser -S -D -u 10001 -G vitals -h /app vitals
WORKDIR /app
COPY --from=backend /out/vitals /out/vitals-migrate /out/vitals-password-hash /usr/local/bin/
COPY migrations ./migrations
COPY docker/entrypoint.sh /usr/local/bin/vitals-entrypoint
RUN chmod 0755 /usr/local/bin/vitals-entrypoint \
    && chown -R vitals:vitals /app
ENTRYPOINT ["/usr/local/bin/vitals-entrypoint"]
