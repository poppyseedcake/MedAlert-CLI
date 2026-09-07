# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG SOURCE_COMMIT=development

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN test "$TARGETOS" = "linux" && test "$TARGETARCH" = "amd64"
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
	go build -trimpath -ldflags="-s -w -X github.com/poppyseedcake/MedAlert/internal/buildinfo.Commit=$SOURCE_COMMIT" \
	-o /out/medalert ./cmd/medalert

FROM --platform=linux/amd64 alpine:3.22

ARG SOURCE_COMMIT=development

RUN apk add --no-cache ca-certificates \
	&& addgroup -S -g 10001 medalert \
	&& adduser -S -D -H -u 10001 -G medalert medalert \
	&& install -d -m 0700 -o 10001 -g 10001 /var/lib/medalert

COPY --from=build --chown=10001:10001 /out/medalert /usr/local/bin/medalert

LABEL org.opencontainers.image.title="MedAlert" \
	org.opencontainers.image.description="Medicover appointment monitoring with Telegram notifications" \
	org.opencontainers.image.source="https://github.com/poppyseedcake/MedAlert" \
	org.opencontainers.image.revision="$SOURCE_COMMIT"

ENV HOME=/var/lib/medalert \
	MEDALERT_DATABASE=/var/lib/medalert/medalert.db \
	MEDALERT_SESSION_DIR=/var/lib/medalert/session \
	MEDALERT_OUTPUT=text \
	MEDALERT_NON_INTERACTIVE=1

VOLUME ["/var/lib/medalert"]
WORKDIR /var/lib/medalert
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/medalert"]
CMD ["watch", "--non-interactive"]
