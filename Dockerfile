FROM golang:1.20

WORKDIR /app

COPY go.sum .
COPY go.mod .

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build . && chmod +x kube-cron-restarter

FROM alpine:3.17

COPY --from=0 /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=0 /app/kube-cron-restarter /kube-cron-restarter

ENTRYPOINT ["/kube-cron-restarter"]