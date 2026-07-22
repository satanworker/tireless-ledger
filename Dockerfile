# syntax=docker/dockerfile:1.7

FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/pi-memoryd .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/pi-memoryd /usr/local/bin/pi-memoryd
EXPOSE 8090
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/pi-memoryd"]
