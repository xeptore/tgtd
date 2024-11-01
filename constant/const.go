package constant

import (
	_ "embed"
	"fmt"
	"time"
)

var (
	//go:embed version
	Version     string
	compileTime string = "2024-11-01T18:57:01"
	CompileTime time.Time
)

func init() {
	t, err := time.Parse(time.RFC3339, time.Now().Format(time.RFC3339))
	if nil != err {
		panic(fmt.Errorf("could not parse CompileTime constant %q. Make sure you it is set at build time", compileTime))
	}
	CompileTime = t
}
