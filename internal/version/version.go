package version

import (
	"runtime/debug"
	"strings"
)

type Info struct {
	Version string `json:"version"`
	GitHash string `json:"git_hash"`
	Dirty   bool   `json:"dirty"`
}

func Current() Info {
	info := Info{Version: "dev"}

	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	if buildInfo.Main.Version != "" && buildInfo.Main.Version != "(devel)" {
		info.Version = buildInfo.Main.Version
	}

	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.GitHash = setting.Value
		case "vcs.modified":
			info.Dirty = setting.Value == "true"
		}
	}

	if info.GitHash != "" && info.Version == "dev" {
		info.Version = ShortHash(info.GitHash)
	}
	return info
}

func ShortHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 7 {
		return hash
	}
	return hash[:7]
}
