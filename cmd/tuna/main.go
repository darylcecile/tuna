package main

import (
	"fmt"
	"os"

	"github.com/darylcecile/tuna/internal/tuna"
)

var version = "dev"

func main() {
	if err := tuna.Execute(version); err != nil {
		fmt.Fprintln(os.Stderr, "tuna:", err)
		os.Exit(1)
	}
}
