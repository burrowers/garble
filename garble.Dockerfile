FROM debian:bookworm AS build-32-arm

ARG TARGETOS
ARG TARGETARCH

RUN apt-get update && apt-get install -y wget build-essential git

RUN wget https://go.dev/dl/go1.25.0.${TARGETOS}-${TARGETARCH}v6l.tar.gz
RUN rm -rf /usr/local/go && tar -C /usr/local -xzf go1.25.0.${TARGETOS}-${TARGETARCH}v6l.tar.gz 
RUN ln -s /usr/local/go/bin/go /usr/bin/go

COPY . /src
WORKDIR /src

RUN go test -short -v ./...
RUN go build -o garble . 

FROM debian:sid AS build

ARG TARGETOS
ARG TARGETARCH

RUN apt-get update && apt-get install -y wget build-essential git

RUN wget https://go.dev/dl/go1.25.0.${TARGETOS}-${TARGETARCH}.tar.gz
RUN rm -rf /usr/local/go && tar -C /usr/local -xzf go1.25.0.${TARGETOS}-${TARGETARCH}.tar.gz 
RUN ln -s /usr/local/go/bin/go /usr/bin/go

COPY . /src
WORKDIR /src

RUN go test -short -v ./...
RUN go build -o garble . 

FROM debian:stable AS garble 
RUN apt-get update && apt-get install -y wget build-essential git
COPY --from=build-64 /src/garble /usr/local/bin/garble
ENTRYPOINT ["/usr/local/bin/garble"]
