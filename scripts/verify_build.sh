#!/bin/sh
set -u
go build ./...
BUILD_EXIT=$?
echo "BUILD_EXIT=$BUILD_EXIT"
go vet ./...
VET_EXIT=$?
echo "VET_EXIT=$VET_EXIT"
exit 0
