#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat <<'EOF'
Build and push the dtk image to Docker Hub.

Usage:
  scripts/release.sh [--repo <namespace/name>] [--tag <tag>]

Options:
  --repo <namespace/name>  Docker Hub repository (default: ylecuyer/dtk)
  --tag <tag>              Image tag (default: latest)
  -h, --help               Show this help

Examples:
  scripts/release.sh
  scripts/release.sh --tag v1.2.0
  scripts/release.sh --repo myuser/dtk --tag v1.2.0
EOF
}

repo="${DOCKERHUB_REPO:-ylecuyer/dtk}"
tag="${DOCKERHUB_TAG:-latest}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      repo="$2"
      shift 2
      ;;
    --tag)
      tag="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required but was not found in PATH" >&2
  exit 1
fi

echo "Building ${repo}:${tag}"
docker build -t "${repo}:${tag}" .

echo "Pushing ${repo}:${tag}"
docker push "${repo}:${tag}"

if [[ "$tag" != "latest" ]]; then
  echo "Tagging and pushing ${repo}:latest"
  docker tag "${repo}:${tag}" "${repo}:latest"
  docker push "${repo}:latest"
fi

echo "Release complete"
