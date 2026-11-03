// Package ecp holds protocol-level definitions shared by the Engram engine:
// the protocol version and the ECP error codes (docs/spec/ecp-v0.md, "Errors").
package ecp

import "fmt"

// ProtocolVersion is the ECP version this implementation speaks.
const ProtocolVersion = "2026-11-01"

// Code is an ECP error code. Codes -32000 to -32019 are reserved by ECP;
// the others are standard JSON-RPC codes.
type Code int

const (
	ParseError         Code = -32700
	InvalidRequest     Code = -32600
	MethodNotFound     Code = -32601
	InvalidParams      Code = -32602
	InternalError      Code = -32603
	NotInitialized     Code = -32001
	UnsupportedVersion Code = -32002
	Unauthorized       Code = -32003
	ScopeDenied        Code = -32004
	MemoryNotFound     Code = -32005
	InvalidProvenance  Code = -32006
	RateLimited        Code = -32007
	ContentRejected    Code = -32008
)

// Error is an error carrying an ECP code. Over the MCP compatibility profile it
// surfaces as a tool error whose text starts with the code, e.g. "[-32004] ...".
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("[%d] %s", int(e.Code), e.Message) }

// Errorf builds an *Error.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
