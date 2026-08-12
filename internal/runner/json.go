package runner

import (
	"encoding/json"
	"fmt"
)

// jsonLine encodes v as a single newline-terminated JSON object.
func jsonLine(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding json: %w", err)
	}
	return append(body, '\n'), nil
}
