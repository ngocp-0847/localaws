# localaws image: the static binary + the docker CLI (runner=docker drives the host engine
# through the mounted /var/run/docker.sock).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /localaws .

FROM alpine:3
RUN apk add --no-cache docker-cli ca-certificates
COPY --from=build /localaws /usr/local/bin/localaws
WORKDIR /var/lib/localaws
EXPOSE 4566
ENV LOCALAWS_DATA=/var/lib/localaws/data
# inside a container, tasks reach the emulator through the host: host.docker.internal
ENTRYPOINT ["localaws"]
