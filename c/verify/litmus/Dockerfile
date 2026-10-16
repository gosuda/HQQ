# Local reproduction of the herdtools7 install used by CI.
#   docker build -t hqq-herd -f c/verify/litmus/Dockerfile c/verify
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && apt-get install -y --no-install-recommends \
      opam ocaml-nox build-essential libgmp-dev pkg-config m4 unzip curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY install-herd.sh /usr/local/bin/install-herd.sh
ARG HERD_JOBS=1
RUN HERD_JOBS=${HERD_JOBS} nice /usr/local/bin/install-herd.sh
