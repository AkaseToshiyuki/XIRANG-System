package main

import (
	"fmt"
	"os"

	"github.com/xirang-grid/xirang/internal/mpiadapter"
)

func main() {
	if err := mpiadapter.Run(os.Args[0], os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(126)
	}
}
