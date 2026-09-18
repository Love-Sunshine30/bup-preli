# ---- build ----
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/gridwise .

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=build /out/gridwise /usr/local/bin/gridwise
USER app
ENV PORT=8080
EXPOSE 8080
# No secrets are baked in. GEMINI_API_KEY must be passed at run time.
ENTRYPOINT ["/usr/local/bin/gridwise"]