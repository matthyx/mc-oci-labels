# Start by building the application.
FROM golang:1.19.1 as build

WORKDIR /go/src/app

COPY go.mod go.sum ./
RUN go mod download

COPY cache ./cache
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /go/bin/app

# Now copy it into our base image.
FROM gcr.io/distroless/static-debian11
COPY --from=build /go/bin/app /
CMD ["/app"]
