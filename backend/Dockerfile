FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -o /out/loadtest ./cmd/loadtest

FROM alpine:3.20
RUN apk add --no-cache tzdata ca-certificates
WORKDIR /app
COPY --from=build /out/api /out/loadtest /app/
EXPOSE 8080
CMD ["/app/api"]
