package main

import (
	"fmt"
	"os"

	"github.com/oldtian123/sing-box-enhanced/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sbe:", err)
		os.Exit(1)
	}
}
