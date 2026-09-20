FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/agent ./cmd/agent
RUN CGO_ENABLED=0 go build -ldflags "-s -w" \
    -o /out/mockbackend ./cmd/mockbackend

FROM alpine:3.20
RUN apk add --no-cache rclone ca-certificates
COPY --from=build /out/agent /usr/local/bin/agent
COPY --from=build /out/mockbackend /usr/local/bin/mockbackend
USER nobody
ENTRYPOINT ["agent"]