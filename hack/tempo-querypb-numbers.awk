# Emits "Message.field number" for the fields of the named messages of an OTLP
# common model, from either the .proto or the .pb.go a Tempo release carries.
# Only the message names in -v only= are reported, so neither file's other
# messages have to parse the same way.

function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }

BEGIN {
  split(only, names, ",")
  for (i in names) wanted[names[i]] = 1
}

# protoc-gen-go struct tags: type Name struct { F T `protobuf:"wire,NUM,...,name=NAME..."` }
# A oneof's cases live in their own wrapper types, named Message_Case, so the
# message is the name up to the first underscore.
FILENAME ~ /\.pb\.go$/ {
  if ($0 ~ /^type[ \t]+[A-Za-z0-9_]+[ \t]+struct[ \t]*\{/) {
    message = $2
    sub(/_.*/, "", message)
    next
  }
  if ($0 ~ /^\}/) { message = ""; next }
  if (message == "" || !(message in wanted)) next
  if (match($0, /protobuf:"[^"]*"/) == 0) next
  tag = substr($0, RSTART + 10, RLENGTH - 11)
  split(tag, part, ",")
  number = part[2]
  field = ""
  for (i in part) if (part[i] ~ /^name=/) { field = part[i]; sub(/^name=/, "", field) }
  if (field != "" && number ~ /^[0-9]+$/) print message "." field " " number
  next
}

# A .proto. Every block of the OTLP common model is a message, so a closing brace
# is a oneof's and the message holding it still owns what follows.
{
  line = $0
  sub(/\/\/.*/, "", line)
  gsub(/\[[^]]*\]/, " ", line)
}
line ~ /^[ \t]*message[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{/ {
  message = line
  sub(/^[ \t]*message[ \t]+/, "", message)
  sub(/[ \t]*\{.*/, "", message)
  message = trim(message)
  next
}
message == "" || !(message in wanted) { next }
line ~ /^[ \t]*(option|reserved|oneof|enum)([ \t]|$)/ { next }
line ~ /=[ \t]*[0-9]+[ \t]*;/ {
  sub(/;.*/, "", line)
  at = index(line, "=")
  number = trim(substr(line, at + 1))
  lhs = trim(substr(line, 1, at - 1))
  parts = split(lhs, token, /[ \t]+/)
  if (parts < 2 || number !~ /^[0-9]+$/) next
  print message "." token[parts] " " number
}
