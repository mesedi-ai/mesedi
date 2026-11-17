package store

// First-seen tool contracts (migration 066). Shared by both stores.

// Kinds of first-seen row. "description" and "definition" match the
// pin kinds (detectors.PinKindDescription, PinKindDefinition) so a pin
// and a first-seen row for the same half of a tool's contract use the
// same name.
const (
	ToolFirstSeenKindTool        = "tool"
	ToolFirstSeenKindDescription = "description"
	ToolFirstSeenKindDefinition  = "definition"
)

// ToolContract is what stored calls say about a tool's contract.
// Empty strings mean no call carried that part.
type ToolContract struct {
	// ExecutionID is the run of the earliest call (EarliestToolContract
	// only); empty when the tool has no stored call.
	ExecutionID string
	// Description is the raw description text; callers hash it with
	// detectors.DescriptionHash, which normalizes whitespace first.
	Description string
	// DescriptionExecutionID is the run the description came from.
	DescriptionExecutionID string
	// SchemaHash is the SDK-computed declared-schema hash.
	SchemaHash string
	// SchemaExecutionID is the run the schema hash came from.
	SchemaExecutionID string
}
