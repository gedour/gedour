FROM node:24-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
COPY web/patches ./patches
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.27-trixie AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION="v0.0.0"
ARG COMMIT_HASH="00000000-dirty"
ARG BUILD_TIMESTAMP="1970-01-01T00:00:00+00:00"
ARG PROJECT_URL="project-name"
ARG PKG="./cmd/gedour"
# Builder=$(go version) is evaluated here: a build arg would report the host's toolchain.
RUN CGO_ENABLED=0 go build -ldflags="-s -w \
    -X 'main.Version=${VERSION}' \
    -X 'main.CommitHash=${COMMIT_HASH}' \
    -X 'main.BuildTimestamp=${BUILD_TIMESTAMP}' \
    -X 'main.Builder=$(go version)' \
    -X 'main.ProjectURL=${PROJECT_URL}'" \
    -o /app "${PKG}"

FROM gcr.io/distroless/static-debian13:nonroot

ARG BUILD_TIMESTAMP="1970-01-01T00:00:00+00:00"
ARG COMMIT_HASH="00000000-dirty"
ARG PROJECT_URL="project-name"
ARG VERSION="v0.0.0"

LABEL org.opencontainers.image.source=${PROJECT_URL}
LABEL org.opencontainers.image.created=${BUILD_TIMESTAMP}
LABEL org.opencontainers.image.version=${VERSION}
LABEL org.opencontainers.image.revision=${COMMIT_HASH}
LABEL org.opencontainers.image.licenses=Apache-2.0

ENV VERSION="${VERSION}" \
    COMMIT_HASH="${COMMIT_HASH}" \
    BUILD_TIMESTAMP="${BUILD_TIMESTAMP}" \
    PROJECT_URL="${PROJECT_URL}"

COPY --from=builder /app /app
EXPOSE 8080 9090
ENTRYPOINT ["/app"]
