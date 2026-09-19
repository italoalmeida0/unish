//go:build windows

package main

import "fmt"

func applyUmask(m int) {}

func ulimitNofile() (uint64, error) {
	return 10240, nil
}

func ulimitSetNofile(n uint64) error {
	return fmt.Errorf("cannot modify limit: operation not permitted")
}
