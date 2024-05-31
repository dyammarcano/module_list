package base

import (
	"log"
	"os"
	"path/filepath"
	"sync"
)

var cwd string
var cwdOnce sync.Once

// UncachedCwd returns the current working directory.
// Most callers should use Cwd, which caches the result for future use.
// UncachedCwd is appropriate to call early in program startup before flag parsing,
// because the -C flag may change the current directory.
func UncachedCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("cannot determine current directory: %v", err)
	}
	return wd
}

// Cwd returns the current working directory at the time of the first call.
func Cwd() string {
	cwdOnce.Do(func() {
		cwd = UncachedCwd()
	})
	return cwd
}

// ShortPath returns an absolute or relative name for path, whatever is shorter.
func ShortPath(path string) string {
	if rel, err := filepath.Rel(Cwd(), path); err == nil && len(rel) < len(path) {
		return rel
	}
	return path
}

func ExitIfErrors() {
	if Errors() > 0 {
		os.Exit(2)
	}
}

var numErrors int

// Errors returns the number of errors reported.
func Errors() int {
	return numErrors
}
