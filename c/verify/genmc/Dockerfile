# Reproducible GenMC image used for local runs of c/verify/genmc.
#   docker build -t hqq-genmc -f c/verify/genmc/Dockerfile c/verify
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && apt-get install -y --no-install-recommends \
      ca-certificates git cmake make bc \
      llvm-19-dev clang-19 libclang-19-dev libffi-dev zlib1g-dev libedit-dev \
    && rm -rf /var/lib/apt/lists/*
COPY install-genmc.sh /usr/local/bin/install-genmc.sh
# GenMCDriver.cpp is memory hungry: keep JOBS low on small Docker VMs. The cache mount keeps the build tree, so a failed build resumes.
ARG JOBS=1
RUN --mount=type=cache,target=/tmp/genmc-src \
    JOBS=${JOBS} nice /usr/local/bin/install-genmc.sh /opt/genmc
ENV PATH=/opt/genmc/bin:$PATH
