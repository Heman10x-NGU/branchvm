FROM node:22-slim

# Intentionally excludes iproute2 and Postgres to exercise pure-Go netlink
# setup and stateless branchvm.toml defaults.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace
