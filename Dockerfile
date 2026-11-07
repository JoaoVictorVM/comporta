# Base images are pinned by digest, so that a retagged upstream image cannot change
# what gets built. The Go version must match go.mod.
FROM golang:1.26.10-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/comporta ./cmd/comporta
# The final image has no shell, so the data directory is prepared here. A new named
# volume inherits this owner and mode.
RUN mkdir -p /out/data && chmod 0700 /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/comporta /comporta
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/comporta"]
CMD ["serve", "--config", "/etc/comporta/comporta.yaml"]
