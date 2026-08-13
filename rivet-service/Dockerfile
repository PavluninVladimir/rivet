# rivetd — control plane одним контейнером: бинарник со встроенной веб-консолью
# (internal/webui/dist, go:embed). Runner'ы в контейнер не входят: они живут
# рядом с рабочими копиями репозиториев и подключаются к gRPC-порту снаружи.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/rivetd ./cmd/rivetd

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/rivetd /usr/local/bin/rivetd
EXPOSE 8080 8090
ENTRYPOINT ["rivetd"]
