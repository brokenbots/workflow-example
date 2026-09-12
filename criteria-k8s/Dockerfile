# syntax=docker/dockerfile:1
FROM golang:1.25 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o manager ./cmd/operator \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o linear-watcher ./cmd/linear-watcher

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager /manager
COPY --from=builder /workspace/linear-watcher /linear-watcher
USER 65532:65532
ENTRYPOINT ["/manager"]
