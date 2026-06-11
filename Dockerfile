# Build a static single binary, run on a minimal distroless base.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled → pure-Go static binary (modernc sqlite, aws-sdk, prometheus all pure-Go)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/inferplane ./cmd/inferplane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/inferplane /usr/local/bin/inferplane
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/inferplane"]
CMD ["serve", "--config", "/etc/inferplane/config.json"]
