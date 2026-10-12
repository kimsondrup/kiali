package tempo

import (
	"fmt"
	"reflect"
	"strings"
)

type operandType string

const (
	AND      operandType = "&&"
	OR       operandType = "||"
	EQUAL    operandType = "="
	NOTEQUAL operandType = "!="
	REGEX    operandType = "=~"
)

type TraceQL struct {
	operator1 interface{}
	operand   operandType
	operator2 interface{}
}

// Groups are ()
type Group struct {
	group   []TraceQL
	operand operandType
}

// Subqueries are {}
type Subquery struct {
	trace TraceQL
}

// unquoted is an operand TraceQL does not take in quotes: the value of an intrinsic is a keyword,
// and the error of "status = error" matches nothing when it is sent as a string.
type unquoted string

// errorTag is the tag Kiali asks a backend for the failed traces of a service with.
const errorTag = "error"

// queryTags ands the tags a query carries onto a TraceQL expression. Both of Tempo's search
// transports build their request through it, so a tag selects the same traces however the request
// travelled - which matters most for the error tag, because tracing.GetErrorTraces counts a
// service's failed traces by asking for it and the count is shown on the service's overview.
//
// The error tag becomes the TraceQL status intrinsic, which Tempo derives from the OTLP span
// status. Measured against a live Tempo over 50 traces of one waypoint: the status intrinsic and
// an error="true" attribute select the same 28 traces, while the OTel SDK spans of the
// application behind that waypoint carry no error attribute at all, so the intrinsic is the only
// one of the two that reads both span sources. The status intrinsic is also the signal the select
// list already asks for and the one the converter mints the error tag from, so the query, the
// answer and the tag agree on one definition of a failed span.
func queryTags(expression TraceQL, tags map[string]string) TraceQL {
	for key, value := range tags {
		tag := TraceQL{operator1: "." + key, operand: EQUAL, operator2: value}
		if key == errorTag && value == "true" {
			tag = TraceQL{operator1: "status", operand: EQUAL, operator2: unquoted("error")}
		}
		expression = TraceQL{operator1: expression, operand: AND, operator2: tag}
	}
	return expression
}

func printOperator(operator interface{}) string {

	queryString := ""
	valueType := reflect.TypeOf(operator)

	switch valueType.String() {
	case "tempo.Subquery":
		queryString = fmt.Sprintf("{ %s }", printOperator(operator.(Subquery).trace))
	case "tempo.Group":
		queryString = "( "
		for i, op := range operator.(Group).group {
			queryString += fmt.Sprintf(" %s ", printOperator(op))
			if i < len(operator.(Group).group)-1 {
				queryString += fmt.Sprintf(" %s ", operator.(Group).operand)
			}
		}
		queryString += ")"
	case "tempo.TraceQL":
		if operator.(TraceQL).operator1 != nil {
			if reflect.TypeOf(operator.(TraceQL).operator2).String() == "string" {
				queryString = fmt.Sprintf("%s %s \"%s\" ", operator.(TraceQL).operator1,
					operator.(TraceQL).operand, operator.(TraceQL).operator2)
			} else {
				if reflect.TypeOf(operator.(TraceQL).operator2).String() == "tempo.unquoted" {
					queryString = fmt.Sprintf("%s %s %s ", operator.(TraceQL).operator1,
						operator.(TraceQL).operand, operator.(TraceQL).operator2)
				} else {
					queryString = fmt.Sprintf("%s %s %s ", printOperator(operator.(TraceQL).operator1),
						operator.(TraceQL).operand, printOperator(operator.(TraceQL).operator2))
				}

			}

		}
	case "string":
		queryString = fmt.Sprintf("%s", operator)
	}
	return queryString
}

func printSelect(fields []string) string {
	selects := strings.Join(fields, ", ")
	return fmt.Sprintf("select(%s)", selects)
}
