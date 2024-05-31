package main

import (
	"context"
	"log"
	"moduleList/internal/modload"
)

func main() {
	moduleName := []string{"github.com/dyammarcano/dataprovider@latest"}

	var mode modload.ListMode

	mode |= modload.ListU | modload.ListRetracted | modload.ListDeprecated | modload.ListVersions

	ctx := context.Background()
	mods, err := modload.ListModules(ctx, moduleName, mode, "")
	if err != nil {
		log.Fatalf("go: %v", err)
	}

	log.Printf("Modules: %v", mods)
}
