# ── build stage: 在容器内编译 warp-go（宿主无需 Go 工具链） ──
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO 纯静态：alpine 无 glibc 也能直接跑；-trimpath/-s/-w 去调试信息缩体积
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/warp .

# ── runtime stage: 免特权、无 TUN、无 NET_ADMIN ──
FROM alpine:latest
# 注册阶段走 HTTPS 到 Cloudflare API，需要根证书
RUN apk add --no-cache ca-certificates
COPY --from=build /out/warp /usr/local/bin/warp
# reg.json 固定落工作目录（见 main.go defaultStateFile）；挂载此目录持久化
WORKDIR /data
# 注册数据由非 root 用户持有（0600）；可通过 docker run --user 覆盖
USER 1001:1001
EXPOSE 40000
ENTRYPOINT ["warp"]
# 默认仅注册；运行时需覆盖此 CMD
CMD ["-reg"]
