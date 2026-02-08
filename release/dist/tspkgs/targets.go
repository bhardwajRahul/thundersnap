package tspkgs

import (
	"fmt"
	"sort"
	"strings"

	_ "github.com/goreleaser/nfpm/v2/deb"
	_ "github.com/goreleaser/nfpm/v2/rpm"
)

// Targets returns all build targets.
func Targets() []Target {
	var ret []Target
	for goosgoarch := range tarballs {
		goos, goarch := splitGoosGoarch(goosgoarch)
		ret = append(ret, &tgzTarget{
			goEnv: map[string]string{"GOOS": goos, "GOARCH": goarch},
		})
	}
	for goosgoarch := range debs {
		goos, goarch := splitGoosGoarch(goosgoarch)
		ret = append(ret, &debTarget{
			goEnv: map[string]string{"GOOS": goos, "GOARCH": goarch},
		})
	}
	for goosgoarch := range rpms {
		goos, goarch := splitGoosGoarch(goosgoarch)
		ret = append(ret, &rpmTarget{
			goEnv: map[string]string{"GOOS": goos, "GOARCH": goarch},
		})
	}
	sort.Slice(ret, func(i, j int) bool {
		return ret[i].String() < ret[j].String()
	})
	return ret
}

var (
	tarballs = map[string]bool{
		"linux/amd64": true,
		"linux/arm64": true,
	}
	debs = map[string]bool{
		"linux/amd64": true,
		"linux/arm64": true,
	}
	rpms = map[string]bool{
		"linux/amd64": true,
		"linux/arm64": true,
	}
)

func splitGoosGoarch(s string) (string, string) {
	goos, goarch, ok := strings.Cut(s, "/")
	if !ok {
		panic(fmt.Sprintf("invalid target %q", s))
	}
	return goos, goarch
}
