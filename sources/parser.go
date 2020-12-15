package sources

import (
	"encoding/json"
	"github.com/jitsucom/eventnative/drivers"
)

func parseFromBytes(b []byte) (map[string]drivers.SourceConfig, error) {
	configs := make(map[string]drivers.SourceConfig)
	err := json.Unmarshal(b, &configs)
	return configs, err
}
