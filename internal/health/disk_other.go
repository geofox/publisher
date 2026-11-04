//go:build !(linux || darwin || freebsd)

package health

func freeBytes(string) (uint64, error) { return 0, errUnsupported }
