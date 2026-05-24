FROM golang:1.26 AS builder
WORKDIR /src
# Dependencies live in the workspace vendor/ at the repo root; `go build`
# auto-selects -mod=vendor when go.work and vendor/ are both present.
COPY . .
RUN CGO_ENABLED=0 go build -C cmd/agc -o /bin/agc .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/agc /agc
ENTRYPOINT ["/agc"]
