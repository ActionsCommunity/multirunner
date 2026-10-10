package consoleapi

import _ "embed"

//go:embed api-contract.schema.json
var contractSchema []byte

// ContractSchema returns a copy of the executable API v1 JSON Schema artifact.
func ContractSchema() []byte {
	return append([]byte(nil), contractSchema...)
}
