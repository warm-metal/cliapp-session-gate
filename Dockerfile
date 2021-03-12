FROM golang:1.15 as builder

WORKDIR /go/src/cliapp-session-gate
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg
RUN CGO_ENABLED=0 go build \
    -o session-gate \
    ./cmd

FROM scratch
COPY --from=builder /go/src/cliapp-session-gate/session-gate .
CMD ["./session-gate"]
