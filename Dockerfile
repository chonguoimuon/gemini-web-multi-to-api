# Stage 1: Build
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Install swag and generate docs
RUN go install github.com/swaggo/swag/cmd/swag@latest
RUN swag init -g ./cmd/server/main.go -o ./cmd/swag/docs --parseDependency --parseInternal

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o main ./cmd/server/main.go

# Stage 2: Final Image
FROM alpine:3.22.2

# Install required packages (including Chromium for Anti-Bot clearance)
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    chromium \
    nss \
    freetype \
    harfbuzz \
    ttf-freefont \
    libstdc++ \
    dbus \
    libdrm \
    mesa-gbm \
    tini

# Create a non-root user and group
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /home/appuser

# Copy file from builder and change ownership
COPY --from=builder --chown=appuser:appgroup /app/main .

# Switch to non-root user
USER appuser

EXPOSE 4982

ENV GOLOG_LOG_LEVEL=info

ENTRYPOINT ["/sbin/tini", "--", "./main"]
