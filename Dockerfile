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
ARG DATE=unknown
ARG LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"

RUN CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o /bin/nanogateway ./cmd/nanogateway

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/nanogateway /usr/local/bin/nanogateway

WORKDIR /workspace

ENTRYPOINT ["nanogateway"]
