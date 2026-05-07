FROM golang:1.23-alpine AS builder

ARG VERSION=dev
ARG BRANCH=unknown

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${VERSION} -X main.Branch=${BRANCH}" -o server ./cmd/server


FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/server .
COPY templates/ templates/
COPY static/    static/

EXPOSE 8080

CMD ["./server"]
