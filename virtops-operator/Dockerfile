# --- Builder stage ---
FROM golang:1.25 as builder
WORKDIR /workspace

ENV CGO_ENABLED=0
ENV GOFLAGS=-mod=vendor

# Copy full source including vendor/
COPY . .

# Build
RUN GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /workspace/manager ./main.go

# --- Final image ---
FROM gcr.io/distroless/static:nonroot
USER nonroot:nonroot
WORKDIR /
COPY --from=builder /workspace/manager /manager
ENTRYPOINT ["/manager"]
