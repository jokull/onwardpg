package protocol

// Diagnostic is the machine-readable envelope for invocation, source,
// configuration, planning, and bundle failures that occur before a normal
// plan Result can be returned.
type Diagnostic struct {
	Status  string `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ErrorDiagnostic(code string, err error) Diagnostic {
	return Diagnostic{Status: "error", Code: code, Message: err.Error()}
}
