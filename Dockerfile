FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o main .

FROM alpine:latest
WORKDIR /root/
# Install CA certificates required for external API calls (Firestore, Gemini)
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/main .
# Cloud Run $PORT is set to 8081
EXPOSE 8081
CMD ["./main"]