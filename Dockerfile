FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /ioswarm-agent .

FROM alpine:3.19
COPY --from=builder /ioswarm-agent /usr/local/bin/
ENTRYPOINT ["ioswarm-agent"]
