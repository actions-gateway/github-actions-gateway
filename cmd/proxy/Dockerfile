FROM golang:1.26 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/proxy .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/proxy /proxy
ENTRYPOINT ["/proxy"]
