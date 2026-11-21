FROM golang:1.26 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /metrics-mux ./cmd/metrics-mux

FROM gcr.io/distroless/static:nonroot
COPY --from=build /metrics-mux /metrics-mux
USER 65532:65532
ENTRYPOINT ["/metrics-mux"]
