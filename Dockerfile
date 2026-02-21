# build stage
FROM golang:1.25 AS build-env
RUN mkdir -p /go/src/github.com/eumel8/rollback-controller
WORKDIR /go/src/github.com/eumel8/rollback-controller
COPY  . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o rollback-controller
# release stage
FROM alpine:latest
RUN adduser -u 10001 -h appuser -D appuser
WORKDIR /appuser
COPY --from=build-env /go/src/github.com/eumel8/rollback-controller .
COPY --from=build-env /etc/passwd /etc/passwd
USER appuser
ENTRYPOINT ["/appuser/rollback-controller"]
