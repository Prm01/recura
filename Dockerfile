# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sieve ./cmd/sieve

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sieve /sieve
COPY examples /examples
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/sieve"]
