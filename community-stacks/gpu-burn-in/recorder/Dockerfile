# Build from this directory: docker build -t gpu-burn-in-recorder .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /recorder .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /recorder /recorder
USER 65532:65532
ENTRYPOINT ["/recorder"]
