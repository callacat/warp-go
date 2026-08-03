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
# 运行时文件（reg.json / config.json / rules.txt / geo）统一落在工作目录下的
# config/ 子目录（应用自动创建，见 core/baseExecRoot + resolveExecPath）：
# 容器内 exe 在只读 /usr/local/bin → 回退到可写的 WORKDIR /data → /data/config。
# compose 把 ./warp-config 挂载到这里即可持久化全部配置。
WORKDIR /data
# 与宿主 uid 1001 对齐：reg.json 由 uid 1001 写、0600，容器 uid 1001 即属主可读
USER 1001:1001
EXPOSE 40000
ENTRYPOINT ["warp"]
# 默认仅注册；compose 用 command 覆盖为真实运行参数
CMD ["-reg"]
