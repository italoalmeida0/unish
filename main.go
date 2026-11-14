package main

import (
	"os"

	"github.com/italoalmeida0/unish/pkg/unish"
)

var version = "dev"

func main() {
	os.Exit(unish.RunCLI(version, os.Args))
}
