# rivetd — control plane одним контейнером: бинарник со встроенной веб-консолью
# (internal/webui/dist, go:embed). Runner'ы в контейнер не входят: они живут
# рядом с рабочими копиями репозиториев и подключаются к gRPC-порту снаружи.
FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Сведения о сборке: версия идёт в состояние установки (GET /system/status),
# версия, коммит и адрес исходников идут в ссылку «Исходный код»
# (GET /api/v1/about). .git в контекст сборки не входит, поэтому коммит
# приходит аргументом: make image передаёт git rev-parse HEAD.
ARG VERSION=dev
ARG COMMIT=
ARG SOURCE_URL=https://github.com/PavluninVladimir/rivet
RUN CGO_ENABLED=0 go build \
    -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.sourceURL=${SOURCE_URL}" \
    -o /out/rivetd ./cmd/rivetd

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates chromium font-noto \
    && addgroup -S -g 10001 rivet \
    && adduser -S -D -H -u 10001 -G rivet -h /nonexistent -s /sbin/nologin rivet
COPY --from=build /out/rivetd /usr/local/bin/rivetd
# Внутри контейнера rivetd слушает все интерфейсы: снаружи его видно только
# через опубликованные порты, а docker-compose публикует их на 127.0.0.1.
# Вне контейнера rivetd по умолчанию слушает только loopback.
ENV RIVET_HTTP_ADDR=0.0.0.0:8080 \
    RIVET_GRPC_ADDR=0.0.0.0:8090
EXPOSE 8080 8090
# Не root: rivetd ничего не пишет на диск (состояние в Postgres, транскрипты
# в S3), каталоги данных ему не нужны.
USER rivet:rivet
ENTRYPOINT ["rivetd"]
