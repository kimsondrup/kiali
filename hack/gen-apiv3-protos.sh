#!/bin/bash
#
# Regenerates the Go stubs for Kiali's trimmed copy of Jaeger's api_v3 query IDL,
# tracing/jaeger/apiv3/query_service.proto, and checks that copy against upstream.
#
# The copy is trimmed by hand from jaegertracing/jaeger-idl's
# proto/api_v3/query_service.proto at JAEGER_IDL_VERSION below. It keeps the three RPCs
# Kiali calls - GetTrace, FindTraces and GetServices - with their request and response
# messages (GetTraceRequest, TraceQueryParameters, FindTracesRequest, GetServicesRequest
# and GetServicesResponse), and of those only the fields Kiali sends; every other field
# number is reserved. Field numbers and types are upstream's. The two searches answer
# with OTLP TracesData, which the published go.opentelemetry.io/proto/otlp module
# describes, so no OTLP type is generated here.
#
# Because the trim is by hand, this script does not trust it: every field the copy
# declares has to appear in upstream's IDL at each release in JAEGER_IDL_VERSIONS with
# the same name, number and type, or nothing is generated.
#
# The IDL and the generated files are committed, so building Kiali needs nothing but a
# Go toolchain. Running this script needs a network.
#
# protoc, the plugins, goimports and the OpenTelemetry protos the IDL imports are fetched
# at pinned versions into a temporary directory, which keeps the generated output
# reproducible and third-party protos out of the repository. Set PROTOC to use a protoc
# already installed instead of fetching one.

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

# The jaeger-idl releases the copy has to agree with, oldest first; the newest is the one
# the copy is trimmed from. v0.6.0 is what Jaeger 2.17.0 (the release Istio 1.31's addon
# ships) builds against, and v0.11.4 is what Jaeger 2.21.0 builds against. Releases before
# v0.6.0 name field 8 of TraceQueryParameters num_traces rather than search_depth, so
# they are left out of this name check; README.adoc records why that rename does not
# matter on the wire.
JAEGER_IDL_VERSIONS=(v0.6.0 v0.11.4 v0.14.0)
JAEGER_IDL_VERSION="${JAEGER_IDL_VERSIONS[*]: -1}"
JAEGER_IDL_PATH="proto/api_v3/query_service.proto"
RAW="https://raw.githubusercontent.com/jaegertracing/jaeger-idl"

GO="${GO:-go}"
ROOTDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROTODIR="${ROOTDIR}/tracing/jaeger/apiv3"
PROTO="${PROTODIR}/query_service.proto"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fetch() {
  if ! curl -sfL --max-time 60 "$1" -o "$2"; then
    echo "fatal: could not fetch $1" >&2
    echo "       $3" >&2
    exit 1
  fi
}

# The fields the copy declares, one "type name = number" per line. Comments, reserved
# lines and options are dropped, so what is compared is only what reaches the wire.
declared_fields() {
  grep -E '^  [A-Za-z0-9.<>, ]+ [a-z_]+ = [0-9]+' "$1" | sed -E 's/^  //; s/ *[;[].*$//'
}

# jaeger-idl is Apache-2.0 as a whole. Assert it rather than trust the note in
# apiv3/README.adoc: a relicence upstream has to stop this script.
echo "Checking jaeger-idl ${JAEGER_IDL_VERSION} is Apache-2.0..."
fetch "${RAW}/${JAEGER_IDL_VERSION}/LICENSE" "${WORKDIR}/LICENSE" \
  "The licence of jaeger-idl cannot be checked, so nothing can be generated."
if ! head -5 "${WORKDIR}/LICENSE" | grep -q "Apache License"; then
  echo "fatal: jaeger-idl ${JAEGER_IDL_VERSION}'s LICENSE is no longer the Apache License:" >&2
  head -5 "${WORKDIR}/LICENSE" | sed 's/^/         /' >&2
  exit 1
fi

declared_fields "${PROTO}" > "${WORKDIR}/declared.txt"
if [ ! -s "${WORKDIR}/declared.txt" ]; then
  echo "fatal: read no fields from ${PROTO#"${ROOTDIR}"/}, so the checks below would prove nothing." >&2
  exit 1
fi

echo "Checking $(wc -l < "${WORKDIR}/declared.txt" | tr -d ' ') fields against jaeger-idl ${JAEGER_IDL_VERSIONS[*]}..."
for version in "${JAEGER_IDL_VERSIONS[@]}"; do
  fetch "${RAW}/${version}/${JAEGER_IDL_PATH}" "${WORKDIR}/${version}.proto" \
    "jaeger-idl ${version} is in JAEGER_IDL_VERSIONS, so its IDL has to be read."
  declared_fields "${WORKDIR}/${version}.proto" > "${WORKDIR}/${version}-declared.txt"
  # A field name such as service_name is unique across the kept messages, so a plain
  # line match is enough; anything the copy declares that upstream does not is drift.
  if ! missing="$(grep -vxF -f "${WORKDIR}/${version}-declared.txt" "${WORKDIR}/declared.txt")"; then
    continue
  fi
  echo "fatal: jaeger-idl ${version} does not declare these fields the way the copy does:" >&2
  echo "${missing}" | sed 's/^/         /' >&2
  echo "       A field that moved to another number or type is a wire incompatibility" >&2
  echo "       nothing else here would notice." >&2
  exit 1
done
echo "  all agree."

echo "Fetching opentelemetry-proto ${OTEL_PROTO_VERSION}..."
curl -sSfL "https://github.com/open-telemetry/opentelemetry-proto/archive/refs/tags/${OTEL_PROTO_VERSION}.tar.gz" | tar xz -C "${WORKDIR}"
OTEL_DIR="${WORKDIR}/opentelemetry-proto-${OTEL_PROTO_VERSION#v}"

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

# The well-known types (duration, timestamp) come from protoc's own include directory,
# which a fetched protoc carries next to its binary.
echo "Generating with $(${PROTOC} --version)..."
PATH="${WORKDIR}/bin:${PATH}" "${PROTOC}" \
  -I "${PROTODIR}" \
  -I "${OTEL_DIR}" \
  --go_out="paths=source_relative:${PROTODIR}" \
  --go-grpc_out="paths=source_relative:${PROTODIR}" \
  query_service.proto

echo "Formatting..."
"${WORKDIR}/bin/goimports" -w -local github.com/kiali/kiali "${PROTODIR}"/*.pb.go

echo "Generated:"
ls -1 "${PROTODIR}"/*.pb.go
