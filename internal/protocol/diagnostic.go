package protocol

const DiagnosticVersion = "onwardpg.diagnostic/v1"

// Diagnostic is the stable machine-readable envelope for invocation, source,
// configuration, planning, and bundle failures that occur before a normal
// plan Result can be returned.
type Diagnostic struct {
	ProtocolVersion string `json:"protocol_version"`
	Status          string `json:"status"`
	Code            string `json:"code"`
	Message         string `json:"message"`
}

func ErrorDiagnostic(code string, err error) Diagnostic {
	return Diagnostic{ProtocolVersion: DiagnosticVersion, Status: "error", Code: code, Message: err.Error()}
}
