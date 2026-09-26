FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /addon ./cmd/addon
RUN mkdir -p /data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /addon /addon
COPY --from=build --chown=nonroot:nonroot /data /data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 CMD ["/addon", "-healthcheck"]
ENTRYPOINT ["/addon"]
