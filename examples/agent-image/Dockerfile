FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    dnsutils \
    git \
    golang-go \
    postgresql-16 \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 22 and Claude Code CLI globally for headless branch runs.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code \
    && npm cache clean --force \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /etc/postgresql/16/main/conf.d \
    && printf "listen_addresses = '127.0.0.1'\nport = 5432\nssl = off\n" > /etc/postgresql/16/main/conf.d/branchvm.conf \
    && printf "local all postgres trust\nlocal all all trust\nhost all all 127.0.0.1/32 trust\nhost all all ::1/128 trust\n" > /etc/postgresql/16/main/pg_hba.conf \
    && chown -R postgres:postgres /etc/postgresql/16/main /var/lib/postgresql \
    && chmod 0700 /var/lib/postgresql/16/main \
    && rm -f /var/lib/postgresql/16/main/postmaster.pid

WORKDIR /workspace
