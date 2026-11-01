FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api .
RUN mkdir -p /out/data

FROM scratch
COPY --from=build /out/api /api
COPY --from=build --chown=65532:65532 /out/data /data
ENV CACHE_DB_PATH=/data/cache.db
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/api"]
