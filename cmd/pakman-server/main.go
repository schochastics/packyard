// Command pakman-server is the pakman package registry server.
//
// See https://github.com/schochastics/pakman for documentation.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/schochastics/pakman/internal/version"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		configPath  = flag.String("config", "", "path to server config file")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	// Placeholder: future phases wire the server start here.
	_ = configPath
	fmt.Fprintln(os.Stderr, "pakman-server: not yet implemented (run with -version)")
	os.Exit(2)
}
