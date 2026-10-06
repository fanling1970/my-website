# 多阶段构建：交叉编译 ARM64（路由器）+ AMD64 通用
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY web ./web
# 默认构建 arm64（京东云雅典娜 AX6600 / IPQ60xx）；其他平台在 Actions 中由 buildx 覆盖
ARG TARGETARCH=arm64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /out/lan-cms .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/lan-cms .
ENV PORT=8090 \
    DATA_DIR=/data \
    TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8090
ENTRYPOINT ["./lan-cms"]
