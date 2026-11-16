FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder

WORKDIR /calshare
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/root/go/pkg \
  go build -o /bin/calshare ./

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
COPY --from=builder /bin/calshare /usr/local/bin/
ENTRYPOINT [ "/usr/local/bin/calshare" ]
