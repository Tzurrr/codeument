package model

import (
	_ "embed"
	"encoding/json"
)

//go:embed draft_schema.json
var draftSchema []byte

// DraftSchema returns the JSON schema the LLM output must satisfy for a Draft.
func DraftSchema() json.RawMessage {
	return json.RawMessage(draftSchema)
}
