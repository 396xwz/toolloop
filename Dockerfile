FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-venv git curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY toolloop /usr/local/bin
WORKDIR /work
