#!/bin/bash
#
# Derives Kiali's copy of Grafana Tempo's query IDL, tracing/tempo/querypb/tempo.proto,
# from upstream's pkg/tempopb/tempo.proto, and regenerates the Go stubs for it.
#
# Nothing in that IDL is written here. hack/tempo-querypb.allow names the service, the
# RPC, the messages and the fields Kiali reads; this script keeps those and nothing
# else, and takes every field number and type from upstream. Moving to a newer Tempo is
# therefore a new entry in TEMPO_VERSIONS and a re-run, not a hand edit.
#
# Every release in TEMPO_VERSIONS has to declare each allow-listed name the same way, so
# the range Kiali supports is checked here rather than claimed in a README.
#
# The IDL and the generated files are committed, so building Kiali needs nothing but a
# Go toolchain. Running this script needs a network, because what it emits is upstream's.
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

# The Tempo releases Kiali's copy of the query IDL has to agree with, oldest first. The
# newest is what the copy is cut from; all of them are asserted to declare every
# allow-listed name with the same number and the same type.
#
# The floor is v2.2.0, which is where StreamingQuerier, TraceSearchMetadata.spanSets and
# SpanSet.attributes first appear and where SearchMetrics last renumbered: put an older
# release in this list and the script names what that release is missing.
TEMPO_VERSIONS=(v2.2.0 v2.3.0 v2.4.0 v2.5.0 v2.6.0 v2.7.0 v2.8.0 v2.9.0 v2.10.0 v3.0.0 v3.1.0)
TEMPO_SOURCE="${TEMPO_VERSIONS[*]: -1}"
# Tempo as a whole is AGPL-3.0 and only some directories are Apache-2.0, so this is both
# where the IDL comes from and what the licence check below asserts.
TEMPO_APACHE_DIR="pkg/tempopb"

# Tempo carries a fork of the OpenTelemetry common model under its own proto package.
# Kiali generates no OTLP type and reads the published model instead, so the fork's types
# are rewritten to it and the numbering of the two is asserted to agree.
OTLP_FROM="tempopb.common.v1."
OTLP_TO="opentelemetry.proto.common.v1."
OTLP_IMPORT="opentelemetry/proto/common/v1/common.proto"
OTLP_MESSAGES="KeyValue,AnyValue"

GO="${GO:-go}"
AWK="${AWK:-awk}"
ROOTDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROTODIR="${ROOTDIR}/tracing/tempo/querypb"
ALLOW="${ROOTDIR}/hack/tempo-querypb.allow"
TRIM="${ROOTDIR}/hack/tempo-querypb-trim.awk"
NUMBERS="${ROOTDIR}/hack/tempo-querypb-numbers.awk"
GO_PACKAGE="github.com/kiali/kiali/tracing/tempo/querypb;querypb"
RAW="https://raw.githubusercontent.com/grafana/tempo"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# Everything this script reads from upstream is required: the IDL it emits is a cut-down
# copy of upstream's, so there is no offline mode in which it could produce anything.
fetch() {
  if ! curl -sfL --max-time 60 "$1" -o "$2"; then
    echo "fatal: could not fetch $1" >&2
    echo "       $3" >&2
    exit 1
  fi
}

# Cuts one release's IDL down to the allow-list. The allow-list is named by its path in
# the repository rather than its absolute one, because the name goes into the generated
# header and into every message this prints.
trim() {
  local version="$1" output="$2"
  if ! "${AWK}" -f "${TRIM}" \
      -v tag="${version}" \
      -v source="${TEMPO_APACHE_DIR}/tempo.proto at ${version}" \
      -v allow="hack/$(basename "${ALLOW}")" \
      -v gopkg="${GO_PACKAGE}" \
      -v otlpFrom="${OTLP_FROM}" \
      -v otlpTo="${OTLP_TO}" \
      -v otlpImport="${OTLP_IMPORT}" \
      "${ALLOW}" "${WORKDIR}/${version}.proto" > "${output}"; then
    echo "fatal: Kiali's IDL cannot be cut from Tempo ${version}, for the reason above." >&2
    echo "       Either hack/$(basename "${ALLOW}") names something that release does not" >&2
    echo "       have, or ${version} does not belong in TEMPO_VERSIONS." >&2
    exit 1
  fi
}

# What a release declares, with everything that legitimately differs between releases
# taken out: the reserved numbers, which are whatever that release has that Kiali does
# not read, and the comments, which upstream rewords. What is left is the names, numbers
# and types the allow-list asks for, so two releases that agree on those compare equal.
declarations() {
  grep -E '^(service|message) |^  ' "$1" | grep -vE '^  (//|reserved)' | sed 's| //.*||'
}

# Tempo is AGPL-3.0 by default and carves out a few directories as Apache-2.0. Only that
# carve-out makes this copy of its IDL usable here, so assert it rather than trust the
# note in querypb/README.adoc: a relicence upstream has to stop this script, not be
# discovered by someone reading a comment.
echo "Checking ${TEMPO_APACHE_DIR} is still Apache-2.0 in Tempo ${TEMPO_SOURCE}..."
LICENSING_URL="${RAW}/${TEMPO_SOURCE}/LICENSING.md"
fetch "${LICENSING_URL}" "${WORKDIR}/LICENSING.md" \
  "The licence of ${TEMPO_APACHE_DIR} cannot be checked, so nothing can be generated."
# Read the directory list under the Apache-2.0 heading and nothing else: the file also
# lists directories under other licences, and a plain grep would match those.
if ! "${AWK}" '/^## Apache-2.0/{f=1; next} f && /^## /{exit} f' "${WORKDIR}/LICENSING.md" \
    | grep -qx "[[:space:]]*${TEMPO_APACHE_DIR}[[:space:]]*"; then
  echo "fatal: ${TEMPO_APACHE_DIR} is no longer listed under Apache-2.0 in Tempo ${TEMPO_SOURCE}." >&2
  echo "       ${LICENSING_URL} says:" >&2
  sed 's/^/         /' "${WORKDIR}/LICENSING.md" >&2
  echo "       Tempo's default licence is AGPL-3.0, which Kiali cannot take. Stopping." >&2
  exit 1
fi
echo "  ${TEMPO_APACHE_DIR} is Apache-2.0."

echo "Fetching Tempo's query IDL at ${#TEMPO_VERSIONS[@]} releases, ${TEMPO_VERSIONS[0]} to ${TEMPO_SOURCE}..."
for version in "${TEMPO_VERSIONS[@]}"; do
  fetch "${RAW}/${version}/${TEMPO_APACHE_DIR}/tempo.proto" "${WORKDIR}/${version}.proto" \
    "Tempo ${version} is in TEMPO_VERSIONS, so its IDL has to be read."
done

echo "Cutting ${TEMPO_SOURCE}'s IDL down to the names hack/$(basename "${ALLOW}") lists..."
trim "${TEMPO_SOURCE}" "${WORKDIR}/tempo.proto"
declarations "${WORKDIR}/tempo.proto" > "${WORKDIR}/declared.txt"
if [ ! -s "${WORKDIR}/declared.txt" ]; then
  echo "fatal: the cut declares nothing, so the checks below would prove nothing." >&2
  exit 1
fi
echo "  $(grep -cE '=' "${WORKDIR}/declared.txt") fields in $(grep -c '^message ' "${WORKDIR}/declared.txt") messages."

echo "Checking every name against all ${#TEMPO_VERSIONS[@]} releases..."
for version in "${TEMPO_VERSIONS[@]}"; do
  trim "${version}" "${WORKDIR}/${version}-cut.proto"
  declarations "${WORKDIR}/${version}-cut.proto" > "${WORKDIR}/${version}-declared.txt"
  if ! diff "${WORKDIR}/declared.txt" "${WORKDIR}/${version}-declared.txt" > "${WORKDIR}/drift.txt"; then
    echo "fatal: Tempo ${version} does not declare these the way ${TEMPO_SOURCE} does" >&2
    echo "       (< is ${TEMPO_SOURCE}, > is ${version}):" >&2
    sed 's/^/         /' "${WORKDIR}/drift.txt" >&2
    echo "       A name that moved to another number, or to another type, is a wire" >&2
    echo "       incompatibility nothing else here would notice. Either stop reading that" >&2
    echo "       field or drop ${version} from TEMPO_VERSIONS and say so in README.adoc." >&2
    exit 1
  fi
done
echo "  ${TEMPO_VERSIONS[0]} to ${TEMPO_SOURCE} all agree."

echo "Fetching opentelemetry-proto ${OTEL_PROTO_VERSION}..."
curl -sSfL "https://github.com/open-telemetry/opentelemetry-proto/archive/refs/tags/${OTEL_PROTO_VERSION}.tar.gz" | tar xz -C "${WORKDIR}"
OTEL_DIR="${WORKDIR}/opentelemetry-proto-${OTEL_PROTO_VERSION#v}"

# Rewriting Tempo's fork of the OTLP common model to the published one is only sound
# while the two number the messages that cross the wire alike. Upstream's fork is
# generated from the OpenTelemetry protos by Tempo's own `make gen-proto`, so it can
# drift from whichever OTLP release go.mod pins, and the wire bytes would then be read
# as the wrong field with no error anywhere.
echo "Checking Tempo's OTLP fork numbers ${OTLP_MESSAGES} as ${OTEL_PROTO_VERSION} does..."
fetch "${RAW}/${TEMPO_SOURCE}/${TEMPO_APACHE_DIR}/common/v1/common.pb.go" "${WORKDIR}/fork.pb.go" \
  "The rewrite to ${OTLP_TO}* cannot be checked, so nothing can be generated."
"${AWK}" -f "${NUMBERS}" -v only="${OTLP_MESSAGES}" "${WORKDIR}/fork.pb.go" | sort > "${WORKDIR}/fork.txt"
"${AWK}" -f "${NUMBERS}" -v only="${OTLP_MESSAGES}" "${OTEL_DIR}/${OTLP_IMPORT}" | sort > "${WORKDIR}/pinned.txt"
if [ ! -s "${WORKDIR}/fork.txt" ] || [ ! -s "${WORKDIR}/pinned.txt" ]; then
  echo "fatal: read no fields from one of the two OTLP common models, so the comparison" >&2
  echo "       proves nothing." >&2
  exit 1
fi
# A name only one of them has is not a conflict: a field Tempo has grown is dropped as
# unknown, and one only the pinned release has never arrives. A name that moved, or a
# number that changed hands, is.
if ! "${AWK}" '
function slot(entry,   message) { message = entry; sub(/\..*/, "", message); return message }
NR == FNR { forkNumber[$1] = $2; forkField[slot($1) "#" $2] = $1; next }
{
  if (($1 in forkNumber) && forkNumber[$1] != $2) {
    print "  " $1 " is " forkNumber[$1] " in Tempo'"'"'s fork and " $2 " in the pinned release"
    conflict = 1
  }
  if ((slot($1) "#" $2) in forkField && forkField[slot($1) "#" $2] != $1) {
    print "  " slot($1) " number " $2 " is " forkField[slot($1) "#" $2] " in Tempo'"'"'s fork and " $1 " in the pinned release"
    conflict = 1
  }
}
END { exit(conflict ? 1 : 0) }
' "${WORKDIR}/fork.txt" "${WORKDIR}/pinned.txt" > "${WORKDIR}/otlp-drift.txt"; then
  echo "fatal: Tempo ${TEMPO_SOURCE} and opentelemetry-proto ${OTEL_PROTO_VERSION} disagree:" >&2
  sed 's/^/       /' "${WORKDIR}/otlp-drift.txt" >&2
  echo "       Rewriting ${OTLP_FROM}* to ${OTLP_TO}* would read Tempo's" >&2
  echo "       attributes as the wrong fields. Move OTEL_PROTO_VERSION, with go.mod." >&2
  exit 1
fi
echo "  $(wc -l < "${WORKDIR}/fork.txt" | tr -d ' ') fields agree."

echo "Writing ${PROTODIR#"${ROOTDIR}"/}/tempo.proto..."
cp "${WORKDIR}/tempo.proto" "${PROTODIR}/tempo.proto"

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

echo "Generating with $(${PROTOC} --version)..."
PATH="${WORKDIR}/bin:${PATH}" "${PROTOC}" \
  -I "${PROTODIR}" \
  -I "${OTEL_DIR}" \
  --go_out="paths=source_relative:${PROTODIR}" \
  --go-grpc_out="paths=source_relative:${PROTODIR}" \
  tempo.proto

echo "Formatting..."
"${WORKDIR}/bin/goimports" -w -local github.com/kiali/kiali "${PROTODIR}"/*.pb.go

echo "Generated:"
ls -1 "${PROTODIR}/tempo.proto" "${PROTODIR}"/*.pb.go
