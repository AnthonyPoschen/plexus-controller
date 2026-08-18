ARG UID=65532
ARG GID=65532

FROM golang:1.26.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o /manager ./cmd/manager

FROM scratch
ARG UID=65532
ARG GID=65532
WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chown=${UID}:${GID} --from=build /manager /manager
USER ${UID}:${GID}
ENTRYPOINT ["/manager"]
