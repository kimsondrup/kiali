#!/bin/bash
#
# Regenerates the Go stubs for Kiali's trimmed copy of Grafana Tempo's query
# IDL, tracing/tempo/querypb/tempo.proto.
#
# The generated files are committed alongside the IDL, so that building Kiali
# needs nothing but a Go toolchain. Run this script after editing the IDL and
# commit what it changes.
#
# protoc, the plugins, goimports and the OpenTelemetry protos the IDL imports
# are fetched at pinned versions into a temporary directory, which keeps the
# generated output reproducible and third-party protos out of the repository.
# Set PROTOC to use a protoc already installed instead of fetching one.

set -euo pipefail

PROTOC_VERSION="36.2"
PROTOC_GEN_GO_VERSION="v1.36.11"
PROTOC_GEN_GO_GRPC_VERSION="v1.5.1"
# goimports groups the generated imports the way `make format` does, so that
# running that target over a fresh generation leaves the tree clean.
GOIMPORTS_VERSION="v0.51.0"
# Keep in step with the go.opentelemetry.io/proto/otlp version in go.mod: the
# generated code imports that module instead of carrying its own OTLP types.
OTEL_PROTO_VERSION="v1.10.0"

GO="${GO:-go}"
ROOTDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROTODIR="${ROOTDIR}/tracing/tempo/querypb"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

if [ -z "${PROTOC:-}" ]; then
  echo "Fetching protoc ${PROTOC_VERSION}..."
  case "$(uname -m)" in
    x86_64) PROTOC_ARCH="x86_64" ;;
    aarch64 | arm64) PROTOC_ARCH="aarch_64" ;;
    *) echo "fatal: no protoc release for $(uname -m); set PROTOC to one you built." >&2; exit 1 ;;
  esac
  PROTOC_ZIP="protoc-${PROTOC_VERSION}-linux-${PROTOC_ARCH}.zip"
  curl -sSfL -o "${WORKDIR}/${PROTOC_ZIP}" \
    "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/${PROTOC_ZIP}"
  unzip -q -d "${WORKDIR}/protoc" "${WORKDIR}/${PROTOC_ZIP}"
  PROTOC="${WORKDIR}/protoc/bin/protoc"
fi

echo "Installing protoc-gen-go ${PROTOC_GEN_GO_VERSION}, protoc-gen-go-grpc ${PROTOC_GEN_GO_GRPC_VERSION} and goimports ${GOIMPORTS_VERSION}..."
GOBIN="${WORKDIR}/bin" ${GO} install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
GOBIN="${WORKDIR}/bin" ${GO} install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"
GOBIN="${WORKDIR}/bin" ${GO} install "golang.org/x/tools/cmd/goimports@${GOIMPORTS_VERSION}"

echo "Fetching opentelemetry-proto ${OTEL_PROTO_VERSION}..."
curl -sSfL "https://github.com/open-telemetry/opentelemetry-proto/archive/refs/tags/${OTEL_PROTO_VERSION}.tar.gz" | tar xz -C "${WORKDIR}"

echo "Generating with $(${PROTOC} --version)..."
PATH="${WORKDIR}/bin:${PATH}" "${PROTOC}" \
  -I "${PROTODIR}" \
  -I "${WORKDIR}/opentelemetry-proto-${OTEL_PROTO_VERSION#v}" \
  --go_out="paths=source_relative:${PROTODIR}" \
  --go-grpc_out="paths=source_relative:${PROTODIR}" \
  tempo.proto

echo "Formatting..."
"${WORKDIR}/bin/goimports" -w -local github.com/kiali/kiali "${PROTODIR}"/*.pb.go

echo "Generated:"
ls -1 "${PROTODIR}"/*.pb.go
