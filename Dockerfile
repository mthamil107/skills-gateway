# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sgw ./cmd/sgw

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sgw /usr/local/bin/sgw
WORKDIR /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sgw"]
CMD ["serve", "-config", "/etc/sgw/sgw.yaml"]
