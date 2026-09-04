FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.24.6-bullseye@sha256:2cdc80dc25edcb96ada1654f73092f2928045d037581fa4aa7c40d18af7dd85a AS builder

ARG SOURCE_DATE_EPOCH
ARG TARGETOS
ARG TARGETARCH

ENV GOPATH=/usr/home/build
ENV GOOS=${TARGETOS}
ENV GOARCH=${TARGETARCH}

RUN mkdir -p /newroot/etc/ssl/certs \
  && cp -ra --parents /etc/ssl/certs /newroot/

WORKDIR /usr/home/build/src

COPY ./src/go.mod ./src/go.sum ./
RUN go mod download

COPY ./src ./
RUN GOPROXY=off \
  CGO_ENABLED=0 \
  go build \
    -o /newroot/usr/local/bin/ecr-proxy \
    ./cmd/ecr-proxy

# Hack to reset timestamps on directories in a multi-platform build
RUN find /newroot -newermt "@${SOURCE_DATE_EPOCH}" -writable \
  | xargs touch --date="@${SOURCE_DATE_EPOCH}" --no-dereference


FROM scratch

LABEL org.opencontainers.image.source=https://github.com/tkhq/ecr-proxy

COPY --from=builder /newroot /

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/ecr-proxy"]
