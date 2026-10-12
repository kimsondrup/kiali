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
# The Tempo release tracing/tempo/querypb/tempo.proto is trimmed from. Tempo as a
# whole is AGPL-3.0 and only some directories are Apache-2.0, so this is both a
# provenance record and what the licence check below asserts.
TEMPO_VERSION="v2.3.0"
TEMPO_APACHE_DIR="pkg/tempopb"

GO="${GO:-go}"
ROOTDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROTODIR="${ROOTDIR}/tracing/tempo/querypb"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# Tempo is AGPL-3.0 by default and carves out a few directories as Apache-2.0. Only
# that carve-out makes this copy of its IDL usable here, so assert it rather than
# trust the note in querypb/README.adoc: a relicence upstream has to stop this script,
# not be discovered by someone reading a comment. The check is skipped, loudly, when
# there is no network, because an offline regeneration of already-vetted protos is
# legitimate; it fails hard only when the answer comes back and the carve-out is gone.
echo "Checking ${TEMPO_APACHE_DIR} is still Apache-2.0 in Tempo ${TEMPO_VERSION}..."
LICENSING_URL="https://raw.githubusercontent.com/grafana/tempo/${TEMPO_VERSION}/LICENSING.md"
if ! curl -sfL --max-time 30 "${LICENSING_URL}" -o "${WORKDIR}/LICENSING.md"; then
  echo "warning: could not fetch ${LICENSING_URL}; the licence of ${TEMPO_APACHE_DIR} is UNVERIFIED." >&2
else
  # Read the directory list under the Apache-2.0 heading and nothing else: the file
  # also lists directories under other licences, and a plain grep would match those.
  if ! awk '/^## Apache-2.0/{f=1; next} f && /^## /{exit} f' "${WORKDIR}/LICENSING.md" \
      | grep -qx "[[:space:]]*${TEMPO_APACHE_DIR}[[:space:]]*"; then
    echo "fatal: ${TEMPO_APACHE_DIR} is no longer listed under Apache-2.0 in Tempo ${TEMPO_VERSION}." >&2
    echo "       ${LICENSING_URL} says:" >&2
    sed 's/^/         /' "${WORKDIR}/LICENSING.md" >&2
    echo "       Tempo's default licence is AGPL-3.0, which Kiali cannot take. Stopping." >&2
    exit 1
  fi
  echo "  ${TEMPO_APACHE_DIR} is Apache-2.0."
fi

# Assert the trimmed IDL still agrees with the one it was trimmed from, rather than
# trusting that it did when it was written. Every field this repo declares must exist
# upstream at the same number: a number that drifts is a wire incompatibility that
# nothing else here would notice, and the generated code's own tests only pin what was
# recorded, not what upstream says. Skipped loudly without a network, for the reason
# the licence check is.
echo "Checking field numbers against Tempo ${TEMPO_VERSION}..."
UPSTREAM_PROTO_URL="https://raw.githubusercontent.com/grafana/tempo/${TEMPO_VERSION}/${TEMPO_APACHE_DIR}/tempo.proto"
if ! curl -sfL --max-time 30 "${UPSTREAM_PROTO_URL}" -o "${WORKDIR}/upstream.proto"; then
  echo "warning: could not fetch ${UPSTREAM_PROTO_URL}; field numbers are UNVERIFIED." >&2
else
  # Emit "Message.field number" for every field of every message. Comments, option
  # lines, bracketed field options and reserved ranges are not fields. A map<k, v>
  # type contains a space, so the name is the token before the "=", not a column.
  cat > "${WORKDIR}/fields.awk" <<'AWK'
{ sub(/\/\/.*/, ""); gsub(/\[[^]]*\]/, "") }
/^[[:space:]]*message[[:space:]]+[A-Za-z_]+[[:space:]]*\{/ {
  match($0, /message[[:space:]]+[A-Za-z_]+/)
  msg = substr($0, RSTART + 8, RLENGTH - 8)
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", msg)
  next
}
/^[[:space:]]*\}/ { msg = ""; next }
msg == "" { next }
/^[[:space:]]*(option|reserved|oneof)\b/ { next }
/=[[:space:]]*[0-9]+[[:space:]]*;/ {
  line = $0
  sub(/;.*/, "", line)
  n = split(line, tok, /[[:space:]]*=[[:space:]]*/)
  if (n < 2) next
  num = tok[2]
  gsub(/[^0-9]/, "", num)
  split(tok[1], lhs, /[[:space:]]+/)
  name = ""
  for (i in lhs) if (lhs[i] != "") name = lhs[i]
  if (name != "" && num != "") print msg "." name " " num
}
AWK
  awk -f "${WORKDIR}/fields.awk" "${PROTODIR}/tempo.proto" | sort > "${WORKDIR}/ours.txt"
  awk -f "${WORKDIR}/fields.awk" "${WORKDIR}/upstream.proto" | sort > "${WORKDIR}/theirs.txt"
  if [ ! -s "${WORKDIR}/ours.txt" ] || [ ! -s "${WORKDIR}/theirs.txt" ]; then
    echo "fatal: parsed no fields from one of the protos, so the comparison proves nothing." >&2
    exit 1
  fi
  DRIFT="$(comm -23 "${WORKDIR}/ours.txt" "${WORKDIR}/theirs.txt")"
  if [ -n "${DRIFT}" ]; then
    echo "fatal: these fields do not match Tempo ${TEMPO_VERSION} at the same number:" >&2
    echo "${DRIFT}" | sed 's/^/         /' >&2
    echo "       ${UPSTREAM_PROTO_URL} is the IDL this one is trimmed from. Either the" >&2
    echo "       trim is wrong, or TEMPO_VERSION needs moving deliberately." >&2
    exit 1
  fi
  echo "  $(wc -l < "${WORKDIR}/ours.txt" | tr -d ' ') fields agree with upstream."
fi

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
