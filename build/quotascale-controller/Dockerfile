FROM golang:1.22-bookworm AS builder

RUN mkdir /app
RUN mkdir /app/conf

ADD . /app/
WORKDIR /app

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main github.com/SSU-DCN/quotascale-controller/cmd/quotascale-controller

FROM scratch

LABEL com.ing.name="quotascale_controller" \
      com.ing.summary="QuotaScale Controller" \
      com.ing.description="Automatically scales ResourceQuotas and coordinates node scaling through QuotaAutoscaler CRDs"

WORKDIR /app/
COPY --from=builder /app/main /app/main
USER 1001

ENTRYPOINT ["./main"]
