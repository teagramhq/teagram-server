FROM mirror.gcr.io/library/golang:1.27 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/mixed-trust-probe/ ./cmd/mixed-trust-probe/
COPY internal/discovery/ ./internal/discovery/
RUN CGO_ENABLED=0 go build -o /mixed-trust-probe ./cmd/mixed-trust-probe

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /mixed-trust-probe /usr/local/bin/mixed-trust-probe
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/mixed-trust-probe"]
