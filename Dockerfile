FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go panel.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/whatsapi .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/whatsapi /app/whatsapi
ENV PORT=8080 TZ=America/Sao_Paulo
VOLUME ["/app/data"]
EXPOSE 8080
CMD ["/app/whatsapi"]
