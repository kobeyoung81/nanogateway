FROM golang:1.24-alpine AS builder

WORKDIR /src
RUN go env -w GO111MODULE=on
RUN go env -w GOPROXY=https://goproxy.cn,direct
RUN apk add --no-cache git
COPY go.mod go.sum ./
COPY go-workwx/ ./go-workwx/
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}"

RUN CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o /bin/nanogateway ./cmd/nanogateway

FROM alpine:3.19

# Build metadata. All three come from the tagged commit (the version tag, the
# commit sha, and that commit's time), so two builds of one tag describe the same
# moment. `created` must be RFC 3339; the build script passes the commit time.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="nanogateway" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://gitee.com/kobeyoung81/nanogateway" \
      org.opencontainers.image.licenses="AGPL-3.0-only"


RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/nanogateway /usr/local/bin/nanogateway

WORKDIR /workspace

ENTRYPOINT ["nanogateway"]
