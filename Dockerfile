# syntax=docker/dockerfile:1
# fogping multi-arch image: linux/amd64, linux/arm64, linux/arm/v7
#
# 与 json-ping 的区别：fogping 依赖 mattn/go-sqlite3（cgo），不能 CGO_ENABLED=0 交叉编译。
# 这里用 tonistiigi/xx：构建机原生架构跑 clang，链接目标架构的 musl，静态产出，不走 QEMU。

ARG GO_VERSION=1.24
ARG XX_VERSION=1.6.1

FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
COPY --from=xx / /
RUN apk add --no-cache clang lld
ARG TARGETPLATFORM
# 目标架构的 C 运行时与头文件（sqlite3.c 编译要用）
RUN xx-apk add --no-cache musl-dev gcc

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=1
# -extldflags -static：musl 全静态，运行层不需要任何 libc
# sqlite_omit_load_extension：静态二进制里 dlopen 无意义，顺便去掉链接警告
RUN xx-go build -trimpath -tags "timetzdata sqlite_omit_load_extension" \
      -ldflags="-s -w -X main.version=${VERSION} -linkmode external -extldflags -static" \
      -o /out/fogping . \
 && xx-verify --static /out/fogping

# busybox:musl 约 1MB：提供 sh（入口脚本）和 wget（健康检查）
FROM busybox:1.37-musl

LABEL org.opencontainers.image.title="fogping" \
      org.opencontainers.image.description="SmokePing-like latency monitor: one binary, embedded SQLite, targets edited in the web UI" \
      org.opencontainers.image.source="https://github.com/githubflyideas/fogping" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/fogping /usr/local/bin/fogping
COPY --chmod=0755 docker/entrypoint.sh /usr/local/bin/docker-entrypoint

# 程序以工作目录为根，数据库落在 ./data/fogping.db → 容器内 /data/data/fogping.db
RUN mkdir -p /data/data && chown -R 65532:65532 /data
WORKDIR /data

ENV TZ=UTC
VOLUME /data
EXPOSE 8518

# 默认非 root；bind mount 时推荐 --user $(id -u):$(id -g) 与宿主目录属主对齐。
# ICMP 用的是非特权 ping socket：Docker 20.10+ 默认在容器 netns 里放开 ping_group_range，无需 NET_RAW。
USER 65532:65532

# /api/version 不经过登录，开启 user=/passwd= 后健康检查依然有效
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:8518/api/version || exit 1

# 参数原样传给 fogping，例如：
#   docker run ... githubflyideas/fogping --edit user=admin passwd=change-me
# 不方便改命令行的场景（Docker Desktop / NAS / RouterOS）用环境变量：
#   FOGPING_EDIT=1  FOGPING_USER=admin  FOGPING_PASSWD=change-me  FOGPING_DAYS=90
ENTRYPOINT ["docker-entrypoint"]
CMD []
