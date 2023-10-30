package debug

import (
	"fmt"
	"os"
	"path"
)

func Init() {
	dir, _ := os.Getwd()
	p := path.Join(dir, "cmd", "device-plugin", "nvidia", "cfg.yaml")
	os.Args = append(os.Args, fmt.Sprintf("--config-file=%s", p))
}
