# build stage
FROM golang:1.26-alpine AS build
WORKDIR /src
# Download deps first for layer caching. go.sum is complete, so module
# verification happens against it without needing the checksum DB.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /calmerge .
# /data holds corrections.json. Created here because distroless has no shell;
# a fresh named volume mounted there inherits this nonroot ownership.
RUN mkdir -p /data

# runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /calmerge /calmerge
COPY --from=build --chown=nonroot:nonroot /data /data
EXPOSE 8076
USER nonroot:nonroot
ENTRYPOINT ["/calmerge"]
