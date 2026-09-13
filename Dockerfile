FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quire-server ./cmd/quire-server \
    && mkdir -p /out/data && chmod 700 /out/data

FROM scratch
COPY --from=build /out/quire-server /quire-server
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV QUIRE_DATA=/data QUIRE_LISTEN=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/quire-server"]
CMD ["serve"]
