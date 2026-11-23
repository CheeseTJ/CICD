# ---------- 构建阶段 ----------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cicd-console .

# ---------- 运行阶段 ----------
FROM alpine:3.20
# JRE 用于鸿蒙 HAP 重签（调 hapsigntoolv2.jar）。
#
# 为什么不挂载宿主机的 JDK：宿主机是 glibc（Ubuntu），本镜像是 musl（alpine），
# 直接挂 /usr/lib/jvm 进去 java 会报 "not found"（动态链接器路径不同）。
# 装 alpine 版的 headless JRE 是唯一稳妥做法，代价约 +90MB。
#
# 只装 headless：签名工具是纯命令行，不需要 AWT/Swing 那套图形依赖。
# 若镜像体积敏感，可改用体积更小的 openjdk8-jre-base，但签名工具建议 17+。
RUN apk add --no-cache openjdk17-jre-headless

# uid/gid 与宿主机 ubuntu 用户一致（1001），便于读写 bind mount 的 ./data
RUN addgroup -g 1001 app && adduser -D -u 1001 -G app app
WORKDIR /app
COPY --from=build /out/cicd-console /app/cicd-console
RUN mkdir -p /app/data /app/hmos && chown -R 1001:1001 /app
USER 1001:1001
ENV PORT=8090 \
    DATA_FILE=/app/data/data.json \
    HMOS_FILE=/app/data/hmos.json \
    HMOS_JAVA=/usr/bin/java
EXPOSE 8090
ENTRYPOINT ["/app/cicd-console"]
