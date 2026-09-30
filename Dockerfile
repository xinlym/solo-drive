# Fixed release tags; update deliberately after reviewing and testing releases.
ARG NODE_VERSION=24.20.0
ARG GO_VERSION=1.27.1

FROM node:${NODE_VERSION}-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

FROM golang:${GO_VERSION}-bookworm AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . ./
COPY --from=frontend /src/web/dist ./web/dist
RUN GOMAXPROCS=2 CGO_ENABLED=0 go build -p 1 -trimpath -ldflags="-s -w" -o /out/solodrive ./cmd/solodrive \
    && mkdir -p /runtime/data \
    && chown 10001:10001 /runtime/data \
    && chmod 0700 /runtime/data

FROM scratch
COPY --from=backend /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=backend --chown=10001:10001 /runtime/data /data
COPY --from=backend /out/solodrive /solodrive
USER 10001:10001
WORKDIR /data
ENV SOLODRIVE_ADDR=0.0.0.0:8091 \
    SOLODRIVE_DATA_DIR=/data \
    SOLODRIVE_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
    SOLODRIVE_COOKIE_SECURE=true
EXPOSE 8091
ENTRYPOINT ["/solodrive"]
