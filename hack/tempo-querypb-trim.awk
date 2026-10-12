# Emits Kiali's copy of Grafana Tempo's query IDL: the allow-listed service, RPCs,
# messages and fields of upstream's, and a "reserved" naming the field upstream
# puts at every number the cut drops. Reads the allow-list, then upstream's IDL.
#
# Imports and the preamble are emitted rather than carried over, so upstream's
# gogoproto, timestamp and trace model imports go without being named here.
#
#   -v tag=        the Tempo release upstream's IDL was read at
#   -v source=     how the generated header names that IDL
#   -v allow=      the allow-list's path, as every message here names it
#   -v gopkg=      the go_package option to inject
#   -v otlpFrom=   the type prefix Tempo's fork of the OTLP model uses
#   -v otlpTo=     the published OTLP prefix it is rewritten to
#   -v otlpImport= the import otlpTo comes from

function die(msg) {
  printf "fatal: %s\n", msg > "/dev/stderr"
  dead = 1
  exit 1
}

function trim(s) {
  gsub(/^[ \t]+|[ \t]+$/, "", s)
  return s
}

function braces(s, c,   n, i) {
  n = 0
  for (i = 1; i <= length(s); i++) if (substr(s, i, 1) == c) n++
  return n
}

function blockName(s, keyword) {
  sub("^" keyword "[ \t]+", "", s)
  sub(/[ \t]*\{.*/, "", s)
  return trim(s)
}

# Every type a kept field or RPC may name: a proto3 scalar, a message the trim
# keeps, or the published OTLP model Tempo's fork is rewritten to. Anything else
# is a Tempo type the trim would have to drag in, so stop rather than emit an IDL
# that does not compile or, worse, one that pulls in another upstream model.
function checkType(t, where,   base, inner, kv) {
  base = trim(t)
  sub(/^(repeated|optional|required)[ \t]+/, "", base)
  sub(/^stream[ \t]+/, "", base)
  if (base ~ /^map<.*>$/) {
    inner = base
    sub(/^map<[ \t]*/, "", inner)
    sub(/[ \t]*>$/, "", inner)
    split(inner, kv, /[ \t]*,[ \t]*/)
    checkType(kv[1], where)
    checkType(kv[2], where)
    return
  }
  if (base in scalar) return
  if (base in keepMessage) return
  if (index(base, otlpTo) == 1) return
  die(where " has type " base ", which is neither a proto3 scalar, a message " \
      allow " keeps, nor " otlpTo "*. Upstream retyped it: either allow-list the " \
      "type it names now or stop reading the field.")
}

function emit(line) {
  body[++nbody] = line
}

BEGIN {
  split("double float int32 int64 uint32 uint64 sint32 sint64 fixed32 fixed64 " \
        "sfixed32 sfixed64 bool string bytes", s, " ")
  for (i in s) scalar[s[i]] = 1
}

# The allow-list: "Service/Rpc" and "Message.field" names, never numbers, because
# a number comes from upstream or not at all.
FNR == NR {
  entry = $0
  sub(/#.*/, "", entry)
  entry = trim(entry)
  if (entry == "") next
  if (entry ~ /^[A-Za-z_][A-Za-z0-9_]*\/[A-Za-z_][A-Za-z0-9_]*$/) {
    owner = entry
    sub(/\/.*/, "", owner)
    keepService[owner] = 1
    keepRPC[entry] = 1
    want[++nwant] = entry
    next
  }
  if (entry ~ /^[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*$/) {
    owner = entry
    sub(/\..*/, "", owner)
    keepMessage[owner] = 1
    keepField[entry] = 1
    want[++nwant] = entry
    next
  }
  die(allow " line " FNR ": \"" entry "\" is neither Service/Rpc nor Message.field")
}

{
  code = $0
  comment = ""
  cut = index(code, "//")
  if (cut > 0) {
    comment = trim(substr(code, cut + 2))
    code = substr(code, 1, cut - 1)
  }
  code = trim(code)
  opens = braces(code, "{")
  closes = braces(code, "}")
  deeper = depth + opens - closes
}

depth == 0 {
  if (code ~ /^syntax[ \t]*=/) {
    syntax = code
    sub(/^syntax[ \t]*=[ \t]*"?/, "", syntax)
    sub(/"?[ \t]*;.*/, "", syntax)
  }
  if (code ~ /^package[ \t]+[A-Za-z0-9_.]+[ \t]*;/) {
    package = code
    sub(/^package[ \t]+/, "", package)
    sub(/[ \t]*;.*/, "", package)
  }
  if (code ~ /^service[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{/) {
    kind = "service"
    block = blockName(code, "service")
    kept = (block in keepService)
    if (kept) {
      if (nbody) emit("")
      emit("service " block " {")
    }
    depth = deeper
    next
  }
  if (code ~ /^message[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{/) {
    kind = "message"
    block = blockName(code, "message")
    kept = (block in keepMessage)
    if (kept) {
      if (nbody) emit("")
      emit("message " block " {")
    }
    statement = ""
    trailing = ""
    npending = 0
    depth = deeper
    next
  }
  # Imports, options, top-level enums and comments: the preamble below is emitted
  # rather than carried over, so upstream's is read for nothing but the syntax.
  depth = deeper
  next
}

# Leaving the service or message.
depth == 1 && deeper == 0 {
  if (kept) {
    if (statement != "") die("unterminated declaration in " block ": " statement)
    emit("}")
  }
  kind = ""
  block = ""
  kept = 0
  depth = 0
  next
}

!kept {
  depth = deeper
  next
}

deeper > 1 {
  die(block " declares a nested block, which this trim does not handle: " code)
}

kind == "service" {
  if (code ~ /^rpc[ \t]/) {
    rpc = code
    sub(/[ \t]*\{[ \t]*\}[ \t]*;?[ \t]*$/, ";", rpc)
    if (rpc !~ /;$/) rpc = rpc ";"
    method = rpc
    sub(/^rpc[ \t]+/, "", method)
    sub(/[ \t]*\(.*/, "", method)
    if ((block "/" method) in keepRPC) {
      found[block "/" method] = 1
      request = rpc
      sub(/^[^(]*\(/, "", request)
      sub(/\).*/, "", request)
      checkType(request, block "/" method "'s request")
      reply = rpc
      sub(/.*returns[ \t]*\(/, "", reply)
      sub(/\).*/, "", reply)
      checkType(reply, block "/" method "'s reply")
      emit("  " rpc)
    }
  }
  depth = deeper
  next
}

kind == "message" {
  if (code == "") {
    # A comment of its own belongs to the declaration below it; a blank line ends
    # that, so it belongs to nothing.
    if (comment != "" && statement == "") pending[++npending] = comment
    else if (comment == "") npending = 0
    next
  }
  statement = (statement == "" ? code : statement " " code)
  if (comment != "" && trailing == "") trailing = comment
  if (index(statement, ";") == 0) next

  declaration = statement
  gsub(/\[[^]]*\]/, " ", declaration)
  sub(/;.*/, "", declaration)
  declaration = trim(declaration)
  note = trailing
  leading = npending
  statement = ""
  trailing = ""
  npending = 0

  if (declaration == "") next
  if (declaration ~ /^option([ \t]|$)/) next
  # A number upstream reserved itself has no field to name, so carry the line over
  # as it stands: it still stops anyone declaring something at that number.
  if (declaration ~ /^reserved([ \t]|$)/) {
    emit("  " declaration ";")
    next
  }
  if (declaration ~ /^(oneof|enum|message)([ \t]|$)/) {
    die(block " declares \"" declaration "\", which this trim does not handle")
  }

  split_at = index(declaration, "=")
  if (split_at == 0) die(block " declares " declaration ", which has no field number")
  number = trim(substr(declaration, split_at + 1))
  if (number !~ /^[0-9]+$/) die(block " declares " declaration ", whose number is not a number")
  lhs = trim(substr(declaration, 1, split_at - 1))
  parts = split(lhs, token, /[ \t]+/)
  if (parts < 2) die(block " declares " declaration ", which has no type")
  field = token[parts]
  type = token[1]
  for (i = 2; i < parts; i++) type = type " " token[i]

  if ((block "." field) in keepField) {
    found[block "." field] = number
    gsub(otlpFrom, otlpTo, type)
    checkType(type, block "." field)
    if (index(type, otlpTo) > 0) otlpUsed = 1
    for (i = 1; i <= leading; i++) emit("  // " pending[i])
    emit("  " type " " field " = " number ";" (note == "" ? "" : " // " note))
  } else {
    # The number stays spoken for and says which field upstream put there, so an
    # upgrade's diff reads as a list of what upstream added and protoc rejects
    # anyone declaring something else at that number.
    emit("  reserved " number "; // " field)
  }
  next
}

{ depth = deeper }

END {
  if (dead) exit 1
  missing = ""
  for (i = 1; i <= nwant; i++) if (!(want[i] in found)) missing = missing "\n         " want[i]
  if (missing != "") {
    print "fatal: " allow " names what Tempo " tag " does not declare:" missing > "/dev/stderr"
    exit 1
  }
  if (package == "") {
    print "fatal: upstream's IDL declares no proto package." > "/dev/stderr"
    exit 1
  }
  # The emitted preamble says proto3, so an upstream that has moved on to another
  # edition has to be read by someone before it is cut down.
  if (syntax != "proto3") {
    print "fatal: upstream's IDL is \"" syntax "\", not proto3." > "/dev/stderr"
    exit 1
  }

  print "// Code generated by hack/gen-tempo-protos.sh. DO NOT EDIT."
  print "//"
  print "// Grafana Tempo's query IDL, " source ", cut down to"
  print "// the names " allow " lists. Every number the cut drops"
  print "// is reserved and named after the field Tempo declares there, so an upgrade's"
  print "// diff reads as a list of what upstream added."
  print "//"
  print "// Tempo's own copy of the OTLP common model is rewritten to the published"
  print "// " otlpTo "*. The generator asserts the two number the"
  print "// attributes alike, so no OTLP type is generated here."
  print "//"
  print "// See tracing/tempo/querypb/README.adoc."
  print ""
  print "syntax = \"proto3\";"
  print ""
  print "package " package ";"
  print ""
  if (otlpUsed) {
    print "import \"" otlpImport "\";"
    print ""
  }
  print "option go_package = \"" gopkg "\";"
  print ""
  for (i = 1; i <= nbody; i++) print body[i]
}
