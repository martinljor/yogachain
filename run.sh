#!/usr/bin/env bash
# Desarrollo: compila y corre el binario local (requiere Go). El usuario final NO
# usa esto: descarga el binario de dist/ (ver build.sh) y lo ejecuta.
set -e
cd "$(dirname "$0")"
go run . "$@"
