# 1) build dashboard (web/dist is gitignored, so it must be built in-image)
FROM node:22-alpine AS web
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# 2) build binary (pure-Go sqlite -> CGO off)
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /app/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yardmaster ./cmd/yardmaster

# 3) runtime
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 1000 yardmaster \
 && mkdir /data && chown yardmaster /data
COPY --from=build /out/yardmaster /usr/local/bin/yardmaster
USER yardmaster
WORKDIR /data
EXPOSE 8787
ENTRYPOINT ["yardmaster"]
CMD ["run", "-c", "config.yaml"]