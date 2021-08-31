#!/bin/bash
SOURCE_PATH=$(realpath "$(dirname "${BASH_SOURCE}")")

echo "src path=""$SOURCE_PATH"
cd "$SOURCE_PATH" || exit
# Do docker build
make
make docker-build