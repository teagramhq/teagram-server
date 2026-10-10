FROM mirror.gcr.io/library/golang:1.27 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/linklanding/ ./cmd/linklanding/
COPY cmd/linkselector/ ./cmd/linkselector/
COPY cmd/linkprobe/ ./cmd/linkprobe/
COPY internal/linklanding/ ./internal/linklanding/
COPY internal/linkselector/ ./internal/linkselector/

RUN CGO_ENABLED=0 go build -trimpath -o /linklanding ./cmd/linklanding \
	&& CGO_ENABLED=0 go build -trimpath -o /linkselector ./cmd/linkselector \
	&& CGO_ENABLED=0 go build -trimpath -o /linkprobe ./cmd/linkprobe

ARG EDGE_IMAGE_SOURCE
ARG EDGE_IMAGE_REVISION
RUN if [ -z "$EDGE_IMAGE_SOURCE" ] && [ -z "$EDGE_IMAGE_REVISION" ]; then \
		exit 0; \
	fi; \
	if [ "$EDGE_IMAGE_SOURCE" != "https://github.com/teagramhq/teagram-server" ]; then \
		echo "invalid edge image provenance" >&2; \
		exit 1; \
	fi; \
	case "$EDGE_IMAGE_REVISION" in \
		*[!0-9a-f]*|'') echo "invalid edge image provenance" >&2; exit 1 ;; \
	esac; \
	if [ "${#EDGE_IMAGE_REVISION}" -ne 40 ]; then \
		echo "invalid edge image provenance" >&2; \
		exit 1; \
	fi

FROM gcr.io/distroless/static-debian12:nonroot AS linklanding
ARG EDGE_IMAGE_SOURCE
ARG EDGE_IMAGE_REVISION
COPY --from=build /linklanding /usr/local/bin/linklanding
COPY --from=build /linkprobe /usr/local/bin/linkprobe
LABEL org.opencontainers.image.source="${EDGE_IMAGE_SOURCE}" \
	org.opencontainers.image.revision="${EDGE_IMAGE_REVISION}"
USER 65532:65532
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/linklanding"]

FROM gcr.io/distroless/static-debian12:nonroot AS selector
ARG EDGE_IMAGE_SOURCE
ARG EDGE_IMAGE_REVISION
COPY --from=build /linkselector /usr/local/bin/linkselector
COPY --from=build /linkprobe /usr/local/bin/linkprobe
LABEL org.opencontainers.image.source="${EDGE_IMAGE_SOURCE}" \
	org.opencontainers.image.revision="${EDGE_IMAGE_REVISION}"
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/linkselector"]
