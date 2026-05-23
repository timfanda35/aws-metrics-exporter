# syntax=docker/dockerfile:1.6

# Stage 1: build
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Cache module downloads independently from source changes.
COPY go.mod go.sum* ./
RUN go mod download

# Copy the rest of the source tree.
COPY . .

# Static, stripped binary suitable for distroless.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /exporter ./cmd/server

# Stage 2: runtime
# distroless/static already ships CA roots and a non-root user (UID 65532).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /exporter /exporter

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/exporter"]
