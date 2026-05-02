#!/bin/bash
# push-to-prod.sh — build local Docker image, SCP to limone, deploy
# Usage: ./scripts/push-to-prod.sh [version_tag]
set -euo pipefail

VERSION="${1:-$(git describe --tags --dirty 2>/dev/null || echo dev)}"
IMAGE_NAME="hikyaku"
REMOTE_USER="hikyaku"
REMOTE_HOST="avocado"
SSH_KEY="$HOME/.ssh/id_deploy"
INSTALL_DIR="~/hikyaku"  # where config lives on the remote
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo ">> Building ${IMAGE_NAME}:${VERSION}"
docker build -t "${IMAGE_NAME}:${VERSION}" .

echo ">> Saving image to tarball"
docker save "${IMAGE_NAME}:${VERSION}" -o "${TMPDIR}/image.tar"
SIZE=$(du -h "${TMPDIR}/image.tar" | cut -f1)
echo "   Image size: ${SIZE}"

echo ">> SCP to ${REMOTE_USER}@${REMOTE_HOST}"
scp -i "${SSH_KEY}" "${TMPDIR}/image.tar" "${REMOTE_USER}@${REMOTE_HOST}:~/image.tar"

echo ">> Loading and deploying on ${REMOTE_HOST}"
ssh -i "${SSH_KEY}" "${REMOTE_USER}@${REMOTE_HOST}" <<SSH
set -e
echo "Loading image..."
docker load -i ~/image.tar
rm -f ~/image.tar

echo "Stopping old container..."
docker stop ${IMAGE_NAME} || true
docker rm ${IMAGE_NAME} || true

echo "Starting new container..."
docker run -d \\
  --name ${IMAGE_NAME} \\
  --restart unless-stopped \\
  -p 4000:4000 \\
  -p 9091:9091 \\
  -v ${INSTALL_DIR}/config.yaml:/config/config.yaml:ro \\\
  -e PROXY_API_KEY \\
  ${IMAGE_NAME}:${VERSION}

sleep 2
echo "Container status:"
docker ps --filter name=${IMAGE_NAME} --format "{{.Names}} {{.Status}}"
SSH

echo "✅ Deployed ${IMAGE_NAME}:${VERSION} to ${REMOTE_HOST}"
