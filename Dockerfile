FROM node:24-alpine AS reader
RUN apk add --no-cache git ca-certificates
ARG QUIRE_READER_REF=b35882bcf8ae2d7a7bb0e7b5531e9c2d5933e7ce
RUN git clone https://github.com/astraldeath/quire.git /reader \
    && cd /reader && git checkout --detach "$QUIRE_READER_REF"
WORKDIR /reader
RUN npm ci && npm run build:web

FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quire-server ./cmd/quire-server \
    && mkdir -p /out/data && chmod 700 /out/data

FROM scratch
COPY LICENSE /licenses/quire-server-AGPL-3.0.txt
COPY --from=reader /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=reader /reader/dist-web /web
COPY --from=reader /reader/LICENSE /licenses/quire-reader-MIT.txt
COPY --from=build /out/quire-server /quire-server
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV QUIRE_DATA=/data QUIRE_LISTEN=0.0.0.0:8080 QUIRE_WEB_DIR=/web
EXPOSE 8080
ENTRYPOINT ["/quire-server"]
CMD ["serve"]
