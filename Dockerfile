FROM golang:1.25 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
	go build -trimpath -ldflags="-s -w" -o /out/mock-kubex-gpu-exporter .

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=build /out/mock-kubex-gpu-exporter /mock-kubex-gpu-exporter

EXPOSE 8080

ENTRYPOINT ["/mock-kubex-gpu-exporter"]
