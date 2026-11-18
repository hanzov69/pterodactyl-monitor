FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /pterodactyl-monitor .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pterodactyl-monitor /pterodactyl-monitor
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/pterodactyl-monitor"]
