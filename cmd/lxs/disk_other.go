//go:build !unix

package main

// diskFree on non-unix platforms reports unknown (0,0): disk self-diagnosis is
// skipped rather than misreported. Keeps go build green on any host.
func diskFree(string) (free, total uint64) { return 0, 0 }
