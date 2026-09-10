# ---------- 构建阶段 ----------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cicd-console .

# ---------- 运行阶段 ----------
FROM alpine:3.20
# uid/gid 与宿主机 ubuntu 用户一致（1001），便于读写 bind mount 的 ./data
RUN addgroup -g 1001 app && adduser -D -u 1001 -G app app
WORKDIR /app
COPY --from=build /out/cicd-console /app/cicd-console
RUN mkdir -p /app/data && chown -R 1001:1001 /app
USER 1001:1001
ENV PORT=8090 \
    DATA_FILE=/app/data/data.json
EXPOSE 8090
ENTRYPOINT ["/app/cicd-console"]
