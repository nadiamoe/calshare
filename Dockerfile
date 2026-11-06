FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS builder

WORKDIR /calshare
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/root/go/pkg \
  go build -o /bin/calshare ./

FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
COPY --from=builder /bin/calshare /usr/local/bin/
ENTRYPOINT [ "/usr/local/bin/calshare" ]
