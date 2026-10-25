//go:build !linux

package storage

func ChownLike(target, source string) error { return nil }
func RequireOwner(path string) error        { return nil }
