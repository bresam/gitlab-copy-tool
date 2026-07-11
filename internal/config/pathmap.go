package config

import (
	"encoding/json"
	"os"
)

// The per-session path map (Session.PathMap) records old-namespace/path ->
// new-namespace/path for every repo migrated within that session. It is used by
// the URL rewrite so references to earlier-migrated repos are fixed too.
//
// LoadPathMapFrom additionally reads an explicit external map (the `--path-map`
// flag), format: a JSON object { "old/group/repo": "new/group/repo", ... }.

// LoadPathMapFrom reads a path map from an explicit file. A missing file yields
// an empty map (not an error).
func LoadPathMapFrom(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}
