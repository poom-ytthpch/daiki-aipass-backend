FROM golang:1.25.13-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/daiki-api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/daiki-api /daiki-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/daiki-api"]
