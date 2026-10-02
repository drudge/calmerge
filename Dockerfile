# build stage. Runs on the builder's own CPU and cross-compiles for the target
# (Go does that natively), so multi-arch images build without emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
# Download deps first for layer caching. go.sum is complete, so module
# verification happens against it without needing the checksum DB.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /calmerge .
# /data holds corrections.json and lessons.json. Created here because
# distroless has no shell; a fresh named volume mounted there inherits this
# nonroot ownership.
RUN mkdir -p /data

# runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /calmerge /calmerge
COPY --from=build --chown=nonroot:nonroot /data /data
EXPOSE 8076
USER nonroot:nonroot
ENTRYPOINT ["/calmerge"]
