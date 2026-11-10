# Stage 1: build the website
FROM node:20-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: build the Go server
FROM golang:1.23-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway

# Stage 3: small image that runs both
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D app
WORKDIR /app
COPY --from=server /out/gateway /app/gateway
COPY --from=web /web/dist /app/web/dist
ENV WEB_DIST=/app/web/dist
USER app
EXPOSE 8080
CMD ["/app/gateway"]