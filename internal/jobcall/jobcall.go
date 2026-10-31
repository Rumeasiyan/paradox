// Package jobcall is the body PARADOX sends when it calls a system's job
// address (contract section 5). The runner writes it; the test system and
// any Go system read it.
package jobcall

import (
	"encoding/json"
	"time"
)

// Call is the JSON body of POST /jobs/<name>.
type Call struct {
	RunID   string          `json:"run_id"`  // same on every attempt of a run
	Job     string          `json:"job"`     // same as the path
	Attempt int             `json:"attempt"` // 1, 2, 3...
	DueAt   time.Time       `json:"due_at"`  // UTC
	Data    json.RawMessage `json:"data"`    // queued data, or null for a scheduled run
}
