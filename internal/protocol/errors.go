package protocol

import "fmt"

// DisconnectReason classifies why a connection was lost or rejected.
// Used for typed errors that drive reconnection strategy.
type DisconnectReason int

const (
	ReasonUnknown          DisconnectReason = iota
	ReasonWrongPassword                     // fatal: wrong password
	ReasonVersionMismatch                   // fatal: incompatible version
	ReasonRoomFull                          // recoverable: wait and retry
	ReasonServerShutdown                    // recoverable: server restarting
	ReasonKickGeneric                       // recoverable: generic kick
)

// DisconnectError is a typed error returned when the server kicks or
// rejects the client. Replaces string-based error classification.
type DisconnectError struct {
	Reason  DisconnectReason
	Message string
	Code    KickCode
}

func (e *DisconnectError) Error() string { return e.Message }

// IsFatal returns true if the error is non-recoverable and the client
// should stop reconnecting.
func (e *DisconnectError) IsFatal() bool {
	return e.Reason == ReasonWrongPassword || e.Reason == ReasonVersionMismatch
}

// NewDisconnectError creates a DisconnectError from a KickPayload.
func NewDisconnectError(kick *KickPayload) *DisconnectError {
	reason := ReasonKickGeneric
	switch kick.Code {
	case KickCodeWrongPassword:
		reason = ReasonWrongPassword
	case KickCodeVersionMismatch:
		reason = ReasonVersionMismatch
	case KickCodeShutdown:
		reason = ReasonServerShutdown
	case KickCodeNone:
		// Legacy server without codes — classify by message content
		reason = classifyKickMessage(kick.Reason)
	}
	return &DisconnectError{
		Reason:  reason,
		Message: kick.Reason,
		Code:    kick.Code,
	}
}

// classifyKickMessage is a fallback for old servers that don't send KickCode.
// Uses keyword matching as a last resort.
func classifyKickMessage(msg string) DisconnectReason {
	for _, kw := range fatalKeywords {
		if containsIgnoreCase(msg, kw) {
			return ReasonWrongPassword
		}
	}
	for _, kw := range versionKeywords {
		if containsIgnoreCase(msg, kw) {
			return ReasonVersionMismatch
		}
	}
	for _, kw := range fullKeywords {
		if containsIgnoreCase(msg, kw) {
			return ReasonRoomFull
		}
	}
	for _, kw := range shutdownKeywords {
		if containsIgnoreCase(msg, kw) {
			return ReasonServerShutdown
		}
	}
	return ReasonUnknown
}

var (
	fatalKeywords    = []string{"password", "密码错误", "wrong password"}
	versionKeywords  = []string{"version", "版本不兼容", "incompatible"}
	fullKeywords     = []string{"room full", "房间已满"}
	shutdownKeywords = []string{"shutdown", "关闭"}
)

func containsIgnoreCase(s, substr string) bool {
	// Simple case-insensitive contains without allocating.
	// For short ASCII keywords this is faster than strings.ToLower.
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			sc := s[i+j]
			kc := substr[j]
			if sc >= 'A' && sc <= 'Z' {
				sc += 32
			}
			if kc >= 'A' && kc <= 'Z' {
				kc += 32
			}
			if sc != kc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// FormatVersion formats an encoded version number (major<<8|minor) as "vX.Y".
func FormatVersion(v uint16) string {
	return fmt.Sprintf("v%d.%d", VersionMajor(v), VersionMinor(v))
}
