# Render container for Remotion (the host is Ubuntu 20.04 / glibc 2.31; Remotion's
# native compositor needs 2.35+). node_modules are mounted from the host (same arch).
#   docker build -t reel-remotion .
#   docker run --rm -v "$PWD":/app -w /app reel-remotion npx remotion render src/index.ts Chapter out/ch01.mp4 --props='{"chapter":"ch01"}'
FROM node:22-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
    libnss3 libdbus-1-3 libatk1.0-0 libgbm1 libasound2 libxrandr2 libxkbcommon0 libxfixes3 \
    libxcomposite1 libxdamage1 libatk-bridge2.0-0 libpango-1.0-0 libcairo2 libcups2 libdrm2 \
    fonts-liberation ca-certificates \
  && rm -rf /var/lib/apt/lists/*
WORKDIR /app
