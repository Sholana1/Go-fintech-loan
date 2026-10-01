# One image definition for every service; SERVICE selects the binary.
#   docker build -f deploy/Dockerfile --build-arg SERVICE=lendingd -t lendingd .
# The provider simulator and development tools are deliberately not buildable
# from this file.
FROM golang:1.27 AS build
ARG SERVICE
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -n "$SERVICE" && case "$SERVICE" in ledgerd|identityd|lendingd) ;; *) echo "unknown service $SERVICE" >&2; exit 1;; esac \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/service ./cmd/${SERVICE}

# Distroless, non-root, no shell: nothing to exec into and nothing to patch
# beyond the binary. The business calendar uses a fixed zone, so no tzdata
# is needed.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
USER nonroot:nonroot
ENTRYPOINT ["/service"]
CMD ["serve"]
