#!/usr/bin/env bash
SOURCE_PATH="$(cd "$(dirname "${BASH_SOURCE}")" && pwd -P)"

echo "src path=""$SOURCE_PATH"
cd "$SOURCE_PATH" || exit
# Do docker build
make
make docker-build