FROM node:24-alpine AS reader
RUN apk add --no-cache git
ARG QUIRE_READER_REF=a85a9c21a326d2e05afe06a5ef1fd02574969cc8
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
COPY --from=reader /reader/dist-web /web
COPY --from=reader /reader/LICENSE /licenses/quire-reader-MIT.txt
COPY --from=build /out/quire-server /quire-server
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV QUIRE_DATA=/data QUIRE_LISTEN=0.0.0.0:8080 QUIRE_WEB_DIR=/web
EXPOSE 8080
ENTRYPOINT ["/quire-server"]
CMD ["serve"]
