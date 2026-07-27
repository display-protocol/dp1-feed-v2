# Build stage
FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY dp1-go /src/dp1-go
COPY dp1-feed-v2 /src/dp1-feed-v2
WORKDIR /src/dp1-feed-v2
RUN CGO_ENABLED=0 go build -o /out/dp1-feed-v2 ./cmd/server

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/dp1-feed-v2 /app/dp1-feed-v2
COPY dp1-feed-v2/config/config.yaml.example /app/config/config.yaml
COPY dp1-feed-v2/db/migrations /app/db/migrations
EXPOSE 8787
ENTRYPOINT ["/app/dp1-feed-v2", "-config", "/app/config/config.yaml", "-migrations", "/app/db/migrations"]
